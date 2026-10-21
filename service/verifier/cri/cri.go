/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package cri

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/service/verifier"
	"github.com/containerd/stargz-snapshotter/util/namedmutex"
	distribution "github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func NewCRIVerifier(
	ctx context.Context,
	config service.Config,
	hosts source.RegistryHosts,
	rootDir string,
	ctdnamespace string,
	connectCRIContainerd func() (runtime.RuntimeServiceClient, runtime.ImageServiceClient, *containerd.Client, error),
	snapshotterGetter func() snapshots.Snapshotter,
) runtime.ImageServiceServer {
	server := &instrumentedService{
		verifyLock: new(namedmutex.NamedMutex),
		connected:  make(chan struct{}),
	}
	go func() {
		log.G(ctx).Debugf("Waiting for CRI service is started...")
		var rc runtime.RuntimeServiceClient
		var ic runtime.ImageServiceClient
		var cc *containerd.Client
		var sn snapshots.Snapshotter
		var err error
		defer close(server.connected)
		for range 100 {
			// wait for all backend services being ready to serve
			if rc == nil {
				rc, ic, cc, err = connectCRIContainerd()
				if err == nil {
					log.G(ctx).Info("connected to backend CRI service")
				} else {
					log.G(ctx).WithError(err).Warnf("failed to connect")
				}
			}
			if sn == nil {
				sn = snapshotterGetter()
				if sn != nil {
					log.G(ctx).Info("snapshotter client is ready")
				} else {
					log.G(ctx).Warnf("snapshotter client is not ready yet")
				}
			}
			if rc != nil && sn != nil {
				server.criMu.Lock()
				server.cri = ic
				server.verifier = verifier.NewVerifier(
					rootDir,
					cc,
					ctdnamespace,
					func(refspec reference.Spec) remotes.Resolver {
						return docker.NewResolver(docker.ResolverOptions{
							Hosts: func(host string) ([]docker.RegistryHost, error) {
								if host != refspec.Hostname() {
									return nil, fmt.Errorf("unexpected host %q for image ref %q", host, refspec.String())
								}
								return hosts(refspec)
							},
						})
					},
					func(ctx context.Context, chainID digest.Digest) error {
						return removeSnapshotsCRI(ctx, cc, rc, ic, sn, chainID)

					},
				)
				server.criMu.Unlock()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
		log.G(ctx).Warnf("no connection is available to the backend")
	}()
	return server
}

type instrumentedService struct {
	runtime.UnimplementedImageServiceServer

	cri   runtime.ImageServiceClient
	criMu sync.Mutex
	// connected is closed once the attempt to connect to the backend is
	// over; cri stays nil if it failed.
	connected chan struct{}

	verifier   *verifier.Verifier
	verifyLock *namedmutex.NamedMutex
}

// getCRI returns the client for the backend CRI service, waiting for the
// connection to be established first. The kubelet validates the image
// service endpoint as soon as it starts, so an immediate error here makes it
// exit instead of retrying.
func (in *instrumentedService) getCRI(ctx context.Context) (runtime.ImageServiceClient, error) {
	select {
	case <-in.connected:
	case <-ctx.Done():
		return nil, fmt.Errorf("server is not initialized yet: %w", ctx.Err())
	}
	in.criMu.Lock()
	defer in.criMu.Unlock()
	if in.cri == nil {
		return nil, errors.New("server is not initialized: the backend CRI service or the snapshotter is not available")
	}
	return in.cri, nil
}

func (in *instrumentedService) ListImages(ctx context.Context, r *runtime.ListImagesRequest) (res *runtime.ListImagesResponse, err error) {
	cri, err := in.getCRI(ctx)
	if err != nil {
		return nil, err
	}
	return cri.ListImages(ctx, r)
}

func (in *instrumentedService) ImageStatus(ctx context.Context, r *runtime.ImageStatusRequest) (res *runtime.ImageStatusResponse, err error) {
	cri, err := in.getCRI(ctx)
	if err != nil {
		return nil, err
	}
	return cri.ImageStatus(ctx, r)
}

func (in *instrumentedService) PullImage(ctx context.Context, r *runtime.PullImageRequest) (res *runtime.PullImageResponse, err error) {
	cri, err := in.getCRI(ctx)
	if err != nil {
		return nil, err
	}
	refspec, err := parseReference(r.GetImage().GetImage())
	if err != nil {
		return nil, err
	}

	manifest, config, err := in.verifier.FetchImageManifest(ctx, refspec)
	if err != nil {
		return nil, err
	}
	diffIDs := config.RootFS.DiffIDs
	layers := manifest.Layers

	// The check works by comparing the existing snapshot against the pulling layer.
	// If there are multiple parallel pullings of layers on a same ChainID before the snapshot
	// is created, they'll pass the check even if their TOCDigests are different. Prevent this
	// by introducing a lock per DiffID.
	lockingDiffIDs := make([]digest.Digest, len(diffIDs))
	copy(lockingDiffIDs, diffIDs)
	sort.Slice(lockingDiffIDs, func(i, j int) bool { return lockingDiffIDs[i].String() < lockingDiffIDs[j].String() })
	for _, d := range lockingDiffIDs {
		in.verifyLock.Lock(d.String())
	}
	defer func() {
		for _, d := range lockingDiffIDs {
			in.verifyLock.Unlock(d.String())
		}
	}()

	ok, err := in.verifier.Verify(ctx, diffIDs, layers)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("invalid chainID")
	}

	return cri.PullImage(ctx, r)
}

func (in *instrumentedService) RemoveImage(ctx context.Context, r *runtime.RemoveImageRequest) (_ *runtime.RemoveImageResponse, err error) {
	cri, err := in.getCRI(ctx)
	if err != nil {
		return nil, err
	}

	return cri.RemoveImage(ctx, r)
}

func (in *instrumentedService) ImageFsInfo(ctx context.Context, r *runtime.ImageFsInfoRequest) (res *runtime.ImageFsInfoResponse, err error) {
	cri, err := in.getCRI(ctx)
	if err != nil {
		return nil, err
	}
	return cri.ImageFsInfo(ctx, r)
}

func parseReference(ref string) (reference.Spec, error) {
	namedRef, err := distribution.ParseDockerRef(ref)
	if err != nil {
		return reference.Spec{}, fmt.Errorf("failed to parse image reference %q: %w", ref, err)
	}
	return reference.Parse(namedRef.String())
}

func removeSnapshotsCRI(ctx context.Context, client *containerd.Client, runtimeClient runtime.RuntimeServiceClient, imageClient runtime.ImageServiceClient, snService snapshots.Snapshotter, chainID digest.Digest) error {
	imgRes, err := imageClient.ListImages(ctx, &runtime.ListImagesRequest{})
	if err != nil {
		return err
	}
	targetImages := make(map[string]*runtime.Image)
	for _, img := range imgRes.Images {
		for _, t := range img.RepoTags {
			i, err := client.GetImage(ctx, t)
			if err != nil {
				continue
			}
			diffIDs, err := i.RootFS(ctx)
			if err != nil {
				return err
			}
			if chainID == identity.ChainID(diffIDs) {
				if _, err := imageClient.RemoveImage(ctx, &runtime.RemoveImageRequest{
					Image: &runtime.ImageSpec{Image: img.Id},
				}); err != nil {
					return err
				}
				targetImages[img.Id] = img
			}
			break
		}
	}

	ctrRes, err := runtimeClient.ListContainers(ctx, &runtime.ListContainersRequest{})
	if err != nil {
		return err
	}
	for _, ctr := range ctrRes.Containers {
		if _, ok := targetImages[ctr.ImageId]; !ok {
			continue
		}
		if _, err := runtimeClient.RemoveContainer(ctx, &runtime.RemoveContainerRequest{ContainerId: ctr.Id}); err != nil {
			return err
		}
	}

	if err := removeSnapshots(ctx, client.SnapshotService("stargz"), chainID.String()); err != nil {
		return fmt.Errorf("failed to remove snapshots: %w", err)
	}
	// containerd core's metadata snapshotter can't synchronously remove a snapshot
	// so we do this explicitly here.
	if err := removeSnapshots(ctx, snService, chainID.String()); err != nil {
		return fmt.Errorf("failed to remove snapshots: %w", err)
	}

	return nil
}

func removeSnapshots(ctx context.Context, sn snapshots.Snapshotter, name string) error {
	var children []string
	sn.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		children = append(children, info.Name)
		return nil
	}, fmt.Sprintf(`parent==%q`, name))
	for _, c := range children {
		if err := removeSnapshots(ctx, sn, c); err != nil {
			return err
		}
	}
	if err := sn.Remove(ctx, name); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("failed to delete invalid snapshot: %w", err)
	}
	return nil
}
