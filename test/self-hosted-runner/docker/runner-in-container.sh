#!/usr/bin/env bash

# This script is run inside the container and it will
# run the actions-runner run.sh script.

if [ ! -d /mnt/actions-runner ]; then
    echo "The /mnt/actions-runner directory not found!"
    exit 1
fi

groupid="$1"
userid="$2"
dockerid="$3"
kvmid="$4"

STOPPED=0
trap ctrl_c INT TERM

ctrl_c() {
    STOPPED=1
}

. /mnt/env

# Create a "runner" user which maps to caller of this script in the host.
groupadd --force --gid "$groupid" runner
useradd --non-unique --create-home --uid "$userid" --gid "$groupid" --groups docker,kvm runner

# Make sure the host docker/kvm group id is the same as what we have in container
# so that docker and qemu commands work as expected
groupmod --non-unique --gid "$dockerid" docker
groupmod --non-unique --gid "$kvmid" kvm

mkdir -p /home/runner/.docker
cat > /home/runner/.docker/config.json <<EOF
{
 "proxies": {
   "default": {
     "httpProxy": "$http_proxy",
     "httpsProxy": "$https_proxy",
     "noProxy": "$no_proxy"
   }
 }
}
EOF

chown -R runner:runner /home/runner/.docker

# Pre-add vagrant box(es) so that we do not need to download them
# TODO: loop here all the images found and import them all
sudo -n -u runner vagrant box add --name generic/fedora37 /mnt/vagrant/generic-fedora37-4.2.14.box

cd /mnt/actions-runner

sudo --preserve-env=http_proxy,https_proxy,no_proxy,HTTP_PROXY,HTTPS_PROXY,NO_PROXY,containerd_version,dns_nameserver,dns_search_domain \
     -n -u runner ./run.sh &

wait

if [ $STOPPED -eq 1 ]; then
    exit 1
fi

exit $?
