package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/golang/glog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

var sanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9]`)

// ResourceConfig represents a single resource to be faked.
type ResourceConfig struct {
	ResourceName string `json:"resourceName"`
	Count        int    `json:"count"`
}

// DummyDeviceManager manages dummy devices for a single resource.
type DummyDeviceManager struct {
	pluginapi.UnimplementedDevicePluginServer
	resourceName string
	devices      map[string]*pluginapi.Device
	socket       string
	server       *grpc.Server
	health       chan *pluginapi.Device
}

// NewDummyDeviceManager creates a new manager for a given resource name and
// device count. Devices are auto-generated with IDs "dev-0", "dev-1", etc.
func NewDummyDeviceManager(resourceName string, count int) *DummyDeviceManager {
	socketName := sanitizeResourceName(resourceName) + ".sock"
	ddm := &DummyDeviceManager{
		resourceName: resourceName,
		devices:      make(map[string]*pluginapi.Device),
		socket:       pluginapi.DevicePluginPath + socketName,
		health:       make(chan *pluginapi.Device),
	}

	for i := 0; i < count; i++ {
		id := fmt.Sprintf("dev-%d", i)
		ddm.devices[id] = &pluginapi.Device{
			ID:     id,
			Health: pluginapi.Healthy,
		}
	}

	glog.Infof("Created manager for %s with %d device(s)", resourceName, count)
	return ddm
}

// sanitizeResourceName replaces non-alphanumeric characters with hyphens,
// producing a string safe for use in socket filenames.
func sanitizeResourceName(name string) string {
	return sanitizeRe.ReplaceAllString(name, "-")
}

// envVarName derives an environment variable name from a resource name.
// For example "nvidia.com/gpu" becomes "DUMMY_DEVICES_NVIDIA_COM_GPU".
func envVarName(resourceName string) string {
	return "DUMMY_DEVICES_" + strings.ToUpper(sanitizeRe.ReplaceAllString(resourceName, "_"))
}

// Start starts the gRPC server of the device plugin.
func (ddm *DummyDeviceManager) Start() error {
	if err := ddm.cleanup(); err != nil {
		return err
	}

	sock, err := net.Listen("unix", ddm.socket)
	if err != nil {
		return err
	}

	ddm.server = grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(ddm.server, ddm)

	go ddm.server.Serve(sock)

	// Wait for server to start by launching a blocking connection.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "unix://"+ddm.socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return err
	}
	conn.Close()

	go ddm.healthcheck()

	return nil
}

// Stop stops the gRPC server.
func (ddm *DummyDeviceManager) Stop() error {
	if ddm.server == nil {
		return nil
	}

	ddm.server.Stop()
	ddm.server = nil

	return ddm.cleanup()
}

// healthcheck periodically logs device status.
// TODO: Implement actual health monitoring.
func (ddm *DummyDeviceManager) healthcheck() {
	for {
		glog.Infof("[%s] devices: %v", ddm.resourceName, ddm.devices)
		time.Sleep(60 * time.Second)
	}
}

func (ddm *DummyDeviceManager) cleanup() error {
	if err := os.Remove(ddm.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Register registers this device plugin with the kubelet for its resource name.
func (ddm *DummyDeviceManager) Register() error {
	conn, err := grpc.Dial("unix://"+pluginapi.KubeletSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("cannot connect to kubelet service: %v", err)
	}
	defer conn.Close()

	socketName := sanitizeResourceName(ddm.resourceName) + ".sock"
	client := pluginapi.NewRegistrationClient(conn)
	req := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     socketName,
		ResourceName: ddm.resourceName,
	}

	_, err = client.Register(context.Background(), req)
	if err != nil {
		return fmt.Errorf("cannot register resource %s with kubelet: %v", ddm.resourceName, err)
	}

	glog.Infof("Registered resource %s with kubelet (endpoint: %s)", ddm.resourceName, socketName)
	return nil
}

// ListAndWatch lists devices and updates that list according to health status.
func (ddm *DummyDeviceManager) ListAndWatch(empty *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	glog.Infof("[%s] ListAndWatch start", ddm.resourceName)
	resp := new(pluginapi.ListAndWatchResponse)
	for _, dev := range ddm.devices {
		resp.Devices = append(resp.Devices, dev)
	}
	if err := stream.Send(resp); err != nil {
		glog.Errorf("[%s] Failed to send ListAndWatch response: %v", ddm.resourceName, err)
	}

	for {
		select {
		case d := <-ddm.health:
			d.Health = pluginapi.Unhealthy
			resp := new(pluginapi.ListAndWatchResponse)
			for _, dev := range ddm.devices {
				resp.Devices = append(resp.Devices, dev)
			}
			if err := stream.Send(resp); err != nil {
				glog.Errorf("[%s] Failed to send ListAndWatch response: %v", ddm.resourceName, err)
			}
		}
	}
}

// Allocate handles device allocation requests from the kubelet.
func (ddm *DummyDeviceManager) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	glog.Infof("[%s] Allocate request", ddm.resourceName)
	responses := pluginapi.AllocateResponse{}
	envName := envVarName(ddm.resourceName)

	for _, req := range reqs.ContainerRequests {
		for _, id := range req.DevicesIds {
			if _, ok := ddm.devices[id]; !ok {
				glog.Errorf("[%s] Can't allocate device %s", ddm.resourceName, id)
				return nil, fmt.Errorf("invalid allocation request for %s: unknown device: %s", ddm.resourceName, id)
			}
		}
		glog.Infof("[%s] Allocated devices: %v", ddm.resourceName, req.DevicesIds)
		response := pluginapi.ContainerAllocateResponse{
			Envs: map[string]string{envName: strings.Join(req.DevicesIds, ",")},
		}
		responses.ContainerResponses = append(responses.ContainerResponses, &response)
	}

	return &responses, nil
}

// GetDevicePluginOptions returns options to be communicated with Device Manager.
func (ddm *DummyDeviceManager) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// PreStartContainer is called before each container start. Device plugin can
// run device specific operations such as resetting the device before making
// devices available to the container.
func (ddm *DummyDeviceManager) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// GetPreferredAllocation returns the preferred allocation from a list of available ones.
func (ddm *DummyDeviceManager) GetPreferredAllocation(context.Context, *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// loadConfig reads and parses the configuration file.
func loadConfig(path string) ([]ResourceConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %v", path, err)
	}
	var resources []ResourceConfig
	if err := json.Unmarshal(raw, &resources); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %v", path, err)
	}
	return resources, nil
}

func main() {
	configPath := flag.String("config", "./dummyResources.json", "Path to the resource configuration file")
	flag.Parse()
	flag.Lookup("logtostderr").Value.Set("true")

	resources, err := loadConfig(*configPath)
	if err != nil {
		glog.Fatal(err)
	}

	if len(resources) == 0 {
		glog.Fatal("No resources defined in configuration")
	}

	var managers []*DummyDeviceManager
	for _, res := range resources {
		if res.Count <= 0 {
			glog.Warningf("Skipping resource %s with count %d", res.ResourceName, res.Count)
			continue
		}
		if res.ResourceName == "" {
			glog.Warning("Skipping resource with empty name")
			continue
		}
		mgr := NewDummyDeviceManager(res.ResourceName, res.Count)
		managers = append(managers, mgr)
	}

	if len(managers) == 0 {
		glog.Fatal("No valid resources to manage after filtering")
	}

	// Handle shutdown signals.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	// Start all managers and register each with kubelet.
	for _, mgr := range managers {
		if err := mgr.Start(); err != nil {
			glog.Fatalf("Could not start device plugin for %s: %v", mgr.resourceName, err)
		}
		glog.Infof("Serving %s on %s", mgr.resourceName, mgr.socket)

		if err := mgr.Register(); err != nil {
			glog.Fatalf("Could not register %s with kubelet: %v", mgr.resourceName, err)
		}
	}

	glog.Infof("All %d device plugin(s) registered successfully", len(managers))

	// Wait for shutdown signal.
	s := <-sigs
	glog.Infof("Received signal \"%v\", shutting down.", s)
	for _, mgr := range managers {
		if err := mgr.Stop(); err != nil {
			glog.Errorf("Error stopping %s: %v", mgr.resourceName, err)
		}
	}
}
