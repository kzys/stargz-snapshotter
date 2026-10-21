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
	"sync/atomic"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/stargz-snapshotter/service"
	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakeImageClient struct {
	runtime.ImageServiceClient
}

func (fakeImageClient) ImageFsInfo(context.Context, *runtime.ImageFsInfoRequest, ...grpc.CallOption) (*runtime.ImageFsInfoResponse, error) {
	return &runtime.ImageFsInfoResponse{}, nil
}

type fakeRuntimeClient struct {
	runtime.RuntimeServiceClient
}

type fakeSnapshotter struct {
	snapshots.Snapshotter
}

// The kubelet validates the image service endpoint right after it starts, so
// requests must wait for the backend connection rather than fail while it is
// still being established.
func TestRequestWaitsForBackend(t *testing.T) {
	var attempts atomic.Int32
	server := NewCRIVerifier(t.Context(), service.Config{}, nil, t.TempDir(), "k8s.io",
		func() (runtime.RuntimeServiceClient, runtime.ImageServiceClient, *containerd.Client, error) {
			if attempts.Add(1) == 1 {
				return nil, nil, nil, errors.New("backend not up yet")
			}
			return fakeRuntimeClient{}, fakeImageClient{}, nil, nil
		},
		func() snapshots.Snapshotter { return fakeSnapshotter{} },
	)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if _, err := server.ImageFsInfo(ctx, &runtime.ImageFsInfoRequest{}); err != nil {
		t.Fatalf("ImageFsInfo: %v", err)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("expected the request to wait for the second connection attempt, got %d attempts", got)
	}
}

func TestRequestGivesUpWithContext(t *testing.T) {
	server := NewCRIVerifier(t.Context(), service.Config{}, nil, t.TempDir(), "k8s.io",
		func() (runtime.RuntimeServiceClient, runtime.ImageServiceClient, *containerd.Client, error) {
			return nil, nil, nil, errors.New("backend not up yet")
		},
		func() snapshots.Snapshotter { return nil },
	)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := server.ImageFsInfo(ctx, &runtime.ImageFsInfoRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error, got %v", err)
	}
}
