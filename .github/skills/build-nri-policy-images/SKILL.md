---
name: build-nri-policy-images
description: "Use when building NRI resource policy container images or regenerating code. Covers `make generate` for code generation and `make images` with appropriate flags for building specific plugin images."
---

# Build NRI Resource Policy Images

This skill covers the two-step build workflow for NRI resource policy plugins: code generation and container image building.

## Step 1: Regenerate Code

After modifying API types, configuration structs, or any file under `pkg/apis/`, run:

```bash
make generate
```

This regenerates:
- `zz_generated.deepcopy.go` files (from types with `+kubebuilder:object:generate=true`)
- CRD YAML manifests in `config/crd/bases/`
- Helm chart CRDs for each plugin in `deployment/helm/*/crds/`
- Client code via `update_codegen.sh`

**Important:** Never edit `zz_generated.deepcopy.go` files manually. Always use `make generate`.

## Step 2: Build Container Images

Build a specific plugin image:

```bash
make PLUGINS=nri-resource-policy-balloons SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= images
```

Built images are saved in `build/images`.

### Key Variables

| Variable | Purpose | Example |
|----------|---------|---------|
| `PLUGINS` | Which plugin(s) to build | `nri-resource-policy-balloons`, `nri-resource-policy-topology-aware` |
| `SKIP_LICENSES` | Skip license scanning (faster dev builds) | `1` |
| `OTHER_IMAGE_TARGETS` | Additional image targets (empty = skip extras) | `` (empty string) |
| `BINARIES` | Which binaries to build (empty = skip standalone) | `` (empty string) |
| `IMAGE_VERSION` | Override image version tag | `v0.12.0-dev` |

### Available Plugins

- `nri-resource-policy-balloons` — Balloons policy
- `nri-resource-policy-topology-aware` — Topology-aware policy
- `nri-resource-policy-template` — Template policy
- `nri-memory-qos` — Memory QoS plugin
- `nri-memtierd` — Memtierd plugin
- `nri-sgx-epc` — SGX EPC plugin

### Build All Plugins

```bash
make images
```

### Build Only Binaries (No Container Images)

```bash
make PLUGINS=nri-resource-policy-balloons binaries
```

## Typical Development Workflow

```bash
# 1. Edit source code (API types, policy logic, etc.)

# 2. Regenerate (if API/config types changed)
make generate

# 3. Build image for your plugin
make PLUGINS=nri-resource-policy-balloons SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images

# 4. Test the image. Typical alternatives are:
#
# 4.1: Run e2e tests (see /run-e2e-tests skill). Tests use the latest image from build/images.
#
# 4.2: Copy (scp) the built image from build/images to the test host.
#      Refer to the helm-launch() function in test/e2e/run.sh to see how to
#      copy helm charts and run `helm install` with local image remotely on the test host.
```

## Troubleshooting

- **Build fails with deepcopy errors**: Run `make generate` first
- **CRD validation errors**: Check that struct tags and kubebuilder markers are correct in `pkg/apis/`
- **Image build hangs**: Check Docker daemon is running; builds use multi-stage Dockerfiles
- **License check fails**: Use `SKIP_LICENSES=1` for development builds
