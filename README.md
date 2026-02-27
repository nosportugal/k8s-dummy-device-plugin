# k8s-dummy-device-plugin

A Kubernetes [Device Plugin](https://kubernetes.io/docs/concepts/cluster-administration/device-plugins/) for **testing purposes only**.

It fakes arbitrary device resources so you can test Kubernetes device scheduling without real hardware. You configure a list of resource names (e.g. `nvidia.com/gpu`, `example.com/fpga`) and a quantity for each. The plugin registers them all with the kubelet, and when a Pod requests those resources, it "allocates" them by injecting environment variables into the container.

## Configuration

Resources are defined in a JSON file (default: `./dummyResources.json`):

```json
[
  {
    "resourceName": "nvidia.com/gpu",
    "count": 4
  },
  {
    "resourceName": "example.com/nic",
    "count": 2
  }
]
```

Each entry creates a separate device plugin server that registers the given `resourceName` with the kubelet and advertises `count` devices.

### Allocation

When a container is allocated devices, the plugin sets an environment variable whose name is derived from the resource name:

| Resource Name     | Environment Variable           | Example Value  |
|-------------------|--------------------------------|----------------|
| `nvidia.com/gpu`  | `DUMMY_DEVICES_NVIDIA_COM_GPU` | `dev-0,dev-1`  |
| `example.com/nic` | `DUMMY_DEVICES_EXAMPLE_COM_NIC`| `dev-0`        |

Device IDs are auto-generated as `dev-0`, `dev-1`, ..., `dev-N`.

## Building

Make sure you have Go installed, then:

```
go build -o k8s-dummy-device-plugin dummy.go
```

Or build the Docker image:

```
docker build -t k8s-dummy-device-plugin .
```

### Command-line Flags

| Flag       | Default                 | Description                    |
|------------|-------------------------|--------------------------------|
| `-config`  | `./dummyResources.json` | Path to the configuration file |

## Deployment (DaemonSet)

Deploy as a DaemonSet so the plugin runs on every node (or a subset of nodes):

```
kubectl apply -f ./examples/daemonset.yml
```

The example DaemonSet includes a ConfigMap with sample resources. Edit the ConfigMap to match your needs.

Then create a Pod that requests the fake resources:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: dummy-pod
spec:
  containers:
    - name: demo-container
      image: busybox
      command: ["sleep", "3600"]
      resources:
        limits:
          nvidia.com/gpu: 2
          example.com/nic: 1
```

Verify the devices were allocated:

```
$ kubectl exec dummy-pod -- printenv | grep DUMMY_DEVICES
DUMMY_DEVICES_NVIDIA_COM_GPU=dev-0,dev-1
DUMMY_DEVICES_EXAMPLE_COM_NIC=dev-0
```

## Testing

End-to-end tests use [kind](https://kind.sigs.k8s.io/) to spin up a local Kubernetes cluster, deploy the plugin, and verify device allocation.

### Prerequisites

- Docker
- [kind](https://kind.sigs.k8s.io/)
- kubectl

### Running locally

```
./test/e2e.sh
```

The script automatically creates a kind cluster, runs the tests, and tears the cluster down on exit (even on failure).

### CI/CD

The E2E tests run automatically on pull requests and pushes to `main`/`master` via the GitHub Actions workflow at `.github/workflows/e2e-test.yml`.

The project also uses GitHub Actions to build and push Docker images to [GitHub Container Registry (GHCR)](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry) via `.github/workflows/build-and-push.yml`.

## License

See [LICENSE](LICENSE).