// Copyright The NRI Plugins Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resmgr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/containers/nri-plugins/pkg/kubernetes/client"
	logger "github.com/containers/nri-plugins/pkg/log"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"google.golang.org/grpc"
	resapi "k8s.io/api/resource/v1beta2"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
)

type draPlugin struct {
	driverName string
	nodeName   string
	resmgr     *resmgr
	cancel     context.CancelFunc
	plugin     *kubeletplugin.Helper
	publishCh  chan<- *resourceslice.DriverResources
}

var (
	dra = logger.NewLogger("dra-driver")
)

func (resmgr *resmgr) publishCPUs(cpuIDs []system.ID) error {
	if resmgr.dra == nil {
		return fmt.Errorf("can't publish CPUs as DRA devices, no DRA plugin")
	}

	err := resmgr.dra.PublishResources(context.Background(), resmgr.system.CPUsAsDRADevices(cpuIDs))
	if err != nil {
		log.Errorf("failed to publish DRA resources: %v", err)
	}
	return err
}

func newDRAPlugin(resmgr *resmgr) (*draPlugin, error) {
	driverName := "dra.cpu"
	driverPath := filepath.Join(kubeletplugin.KubeletPluginsDir, driverName)
	if err := os.MkdirAll(driverPath, 0750); err != nil {
		return nil, fmt.Errorf("failed to create driver directory %s: %w", driverPath, err)
	}

	return &draPlugin{
		driverName: driverName,
		nodeName:   resmgr.agent.NodeName(),
		resmgr:     resmgr,
	}, nil
}

func (p *draPlugin) start() error {
	p.run()
	return nil
}

func (p *draPlugin) run() {
	var (
		ctx, cancel = context.WithCancel(context.Background())
		publishCh   = make(chan *resourceslice.DriverResources, 1)
	)

	go func() {
		for {
			var resources *resourceslice.DriverResources

			select {
			case <-ctx.Done():
				return
			case r, ok := <-publishCh:
				if !ok {
					return
				}
				resources = r
			}

			if p.plugin == nil {
				if err := p.connect(); err != nil {
					log.Errorf("failed start DRA plugin: %v", err)
					continue
				}
			}

			if p.plugin != nil {
				if err := p.plugin.PublishResources(ctx, *resources); err != nil {
					log.Errorf("failed to publish DRA resources: %v", err)
				} else {
					log.Infof("published DRA resources, using %d pool(s)...", len(resources.Pools))
				}
			}

			resources = nil
		}
	}()

	p.cancel = cancel
	p.publishCh = publishCh
}

func (p *draPlugin) connect() error {
	kubeClient, err := client.New(
		client.WithKubeOrInClusterConfig(p.resmgr.agent.KubeConfig()),
		client.WithAcceptContentTypes(client.ContentTypeProtobuf, client.ContentTypeJSON),
		client.WithContentType(client.ContentTypeProtobuf),
	)
	if err != nil {
		return fmt.Errorf("can't create kube client for DRA plugin: %w", err)
	}

	options := []kubeletplugin.Option{
		kubeletplugin.DriverName(p.driverName),
		kubeletplugin.NodeName(p.nodeName),
		kubeletplugin.KubeClient(kubeClient.Clientset),
		kubeletplugin.GRPCInterceptor(p.unaryInterceptor),
	}

	log.Infof("using DRA driverName=%s nodeName=%s", p.driverName, p.nodeName)

	plugin, err := kubeletplugin.Start(context.Background(), p, options...)
	if err != nil {
		return fmt.Errorf("failed to start DRA plugin: %w", err)
	}

	p.plugin = plugin
	return nil
}

func (p *draPlugin) stop() {
	if p == nil {
		return
	}

	if p.plugin != nil {
		p.plugin.Stop()
	}
	if p.cancel != nil {
		p.cancel()
	}

	p.plugin = nil
	p.cancel = nil
}

func (p *draPlugin) unaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	dra.Info("=> gRPC call %v, handler %v\n", *info, handler)
	rpl, err := handler(ctx, req)
	dra.Info("<= gRPC reply: %+v, %v\n", rpl, err)
	return rpl, err
}

func (p *draPlugin) IsRegistered() (bool, error) {
	if p == nil || p.plugin == nil {
		return false, errors.New("DRA plugin is not initialized")
	}

	status := p.plugin.RegistrationStatus()
	if status == nil {
		return false, nil
	}

	var err error
	if status.Error != "" {
		err = errors.New(status.Error)
	}

	return status.PluginRegistered, err
}

func (p *draPlugin) PublishResources(ctx context.Context, devices []resapi.Device) error {
	resources := resourceslice.DriverResources{
		Pools: make(map[string]resourceslice.Pool),
	}

	maxPerPool := resapi.ResourceSliceMaxDevices
	for n := len(devices); n > 0; n = len(devices) {
		if n > maxPerPool {
			n = maxPerPool
		}
		resources.Pools["pool"+strconv.Itoa(len(resources.Pools))] = resourceslice.Pool{
			Slices: []resourceslice.Slice{
				{
					Devices: devices[:n],
				},
			},
		}
		devices = devices[n:]
	}

	log.Infof("publishing DRA resources, using %d pool(s)...", len(resources.Pools))

	select {
	case p.publishCh <- &resources:
		return nil
	default:
	}

	return fmt.Errorf("failed to publish resources, failed to send on channel")
}

func (p *draPlugin) PrepareResourceClaims(ctx context.Context, claims []*resapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	log.Infof("should prepare %d claims:", len(claims))

	result := make(map[types.UID]kubeletplugin.PrepareResult)

	for i, c := range claims {
		if c == nil {
			continue
		}

		dra.Debug("  - claim #%d:", i)
		specHdr := fmt.Sprintf("    <claim #%d spec> ", i)
		statusHdr := fmt.Sprintf("    <claim #%d status> ", i)

		dra.DebugBlock(specHdr, "%s", logger.AsYaml(c.Spec))
		dra.DebugBlock(statusHdr, "%s", logger.AsYaml(c.Status))

		r := kubeletplugin.PrepareResult{}
		for _, a := range c.Status.Allocation.Devices.Results {
			r.Devices = append(r.Devices,
				kubeletplugin.Device{
					Requests:     []string{a.Request},
					DeviceName:   a.Device,
					CDIDeviceIDs: []string{"dra.cpu/core=" + a.Device},
				})
		}
		result[c.UID] = r
	}

	return result, nil
}

func (p *draPlugin) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	log.Infof("should un-prepare %d claims:", len(claims))

	result := make(map[types.UID]error)

	for _, c := range claims {
		log.Infof("  - un-claim %+v", c)

		result[c.UID] = nil
	}

	return result, nil
}

func (p *draPlugin) ErrorHandler(ctx context.Context, err error, msg string) {
	log.Errorf("resource slice publishing error: %v (%s)", err, msg)
}
