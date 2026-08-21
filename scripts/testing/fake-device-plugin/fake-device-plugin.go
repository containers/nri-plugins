package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"flag"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"sigs.k8s.io/yaml"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	pluginDir = "/var/lib/kubelet/device-plugins"
)

type Config struct {
	ResourceName string         `yaml:"resourceName"`
	Devices      []DeviceConfig `yaml:"devices"`
}

type DeviceConfig struct {
	ID        string  `yaml:"id"`
	NUMANodes []int64 `yaml:"numaNodes,omitempty"`
}

type FakeDevicePlugin struct {
	pluginapi.UnimplementedDevicePluginServer

	resourceName string
	devices      []*pluginapi.Device
	stop         chan any
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func newPlugin(cfg *Config) *FakeDevicePlugin {
	p := &FakeDevicePlugin{
		resourceName: cfg.ResourceName,
		stop:         make(chan any),
	}

	for _, d := range cfg.Devices {
		dev := &pluginapi.Device{
			ID:     d.ID,
			Health: pluginapi.Healthy,
		}
		if len(d.NUMANodes) > 0 {
			numaNodes := []*pluginapi.NUMANode{}
			for _, node := range d.NUMANodes {
				numaNodes = append(numaNodes, &pluginapi.NUMANode{ID: node})
			}
			dev.Topology = &pluginapi.TopologyInfo{
				Nodes: numaNodes,
			}
		}
		p.devices = append(p.devices, dev)
	}
	log.Printf("newPlugin: %v[%v]", p.resourceName, p.devices)

	return p
}

func (p *FakeDevicePlugin) GetDevicePluginOptions(
	context.Context,
	*pluginapi.Empty,
) (*pluginapi.DevicePluginOptions, error) {

	return &pluginapi.DevicePluginOptions{}, nil
}

func (p *FakeDevicePlugin) ListAndWatch(
	_ *pluginapi.Empty,
	stream pluginapi.DevicePlugin_ListAndWatchServer,
) error {

	log.Printf("ListAndWatch: stream output %v", p.devices)

	_ = stream.Send(&pluginapi.ListAndWatchResponse{
		Devices: p.devices,
	})

	<-p.stop
	return nil
}

func (p *FakeDevicePlugin) Allocate(
	ctx context.Context,
	req *pluginapi.AllocateRequest,
) (*pluginapi.AllocateResponse, error) {

	log.Printf("Allocate input: %s", req.String())
	resp := &pluginapi.AllocateResponse{}

	for range req.ContainerRequests {
		resp.ContainerResponses = append(
			resp.ContainerResponses,
			&pluginapi.ContainerAllocateResponse{},
		)
	}
	log.Printf("Allocate output: %s", resp.String())

	return resp, nil
}

func (p *FakeDevicePlugin) PreStartContainer(
	_ context.Context,
	req *pluginapi.PreStartContainerRequest,
) (*pluginapi.PreStartContainerResponse, error) {

	log.Printf("PreStartContainer input: %s", req.String())

	return &pluginapi.PreStartContainerResponse{}, nil
}

func register(socket, resource string) error {

	conn, err := grpc.NewClient(
		"unix://"+pluginapi.KubeletSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	client := pluginapi.NewRegistrationClient(conn)

	_, err = client.Register(
		context.Background(),
		&pluginapi.RegisterRequest{
			Version:      pluginapi.Version,
			Endpoint:     socket,
			ResourceName: resource,
		},
	)

	return err
}

func main() {
	log.SetPrefix("fake-device-plugin<> ")
	configPath := flag.String("config", "fake-device-plugin.yaml", "Path to device plugin config file")
	socketPath := flag.String("socket", "", "Unix socket path (optional, defaults to <pluginDir>/fake-device-plugin-<resourceName>.sock)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	log.SetPrefix(fmt.Sprintf("fake-device-plugin<%s> ", cfg.ResourceName))

	plugin := newPlugin(cfg)

	var socket string
	var socketPathAbs string
	if *socketPath != "" {
		socket = filepath.Base(*socketPath)
		socketPathAbs = *socketPath
	} else {
		socket = fmt.Sprintf("fake-device-plugin-%s.sock", strings.ReplaceAll(cfg.ResourceName, "/", "-"))
		socketPathAbs = filepath.Join(pluginDir, socket)
	}

	_ = os.Remove(socketPathAbs)

	lis, err := net.Listen("unix", socketPathAbs)
	if err != nil {
		log.Fatal(err)
	}

	grpcServer := grpc.NewServer()

	pluginapi.RegisterDevicePluginServer(grpcServer, plugin)

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("grpcServer.Serve error: %v", err)
		}
	}()

	if err := register(socket, cfg.ResourceName); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("registered on %s\n", socketPathAbs)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	<-sigChan

	fmt.Println("shutting down...")
	grpcServer.Stop()
	_ = os.Remove(socketPathAbs)
}
