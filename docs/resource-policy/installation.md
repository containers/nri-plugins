# Installation

## Installing from sources

You will need at least `git`, {{ '`golang '+ '{}'.format(golang_version) + '`' }} or newer,
`GNU make`, `bash`, `find`, `sed`, `head`, `date`, and `install` to be able to build and install
from sources.

Although not recommended, you can install NRI Resource Policy from sources:

```console
  git clone https://github.com/containers/nri-plugins
  make && make images
```

After the images are created, you can copy the tar images from `build/images` to
the target device and deploy the relevant DaemonSet deployment file found also
in images directory.

For example, you can deploy topology-aware resource policy like this:

```console
  cd build/images
  ctr -n k8s.io image import nri-resource-policy-topology-aware-image-321ca3aad95e.tar
  kubectl apply -f nri-resource-policy-topology-aware-deployment.yaml
```
