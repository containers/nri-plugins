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

CONF_DIR=/mnt/conf

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

# We want Qemu networking to contact our special dnsmasq resolver so that we
# can catch special domain queries. So set the /etc/resolv.conf in the
# container to point to localhost, and configure dnsmasq with the dns
# that points to outside world.

cp /etc/resolv.conf $CONF_DIR/resolv.conf.orig

echo "nameserver 127.0.0.1" > /etc/resolv.conf
echo "search runner" >> /etc/resolv.conf

LOCALIP=$(ip addr show dev eth0 | awk '/inet / { print $2 }' | cut -f1 -d/)
sed -i "s/LOCALIP/$LOCALIP/" $CONF_DIR/dnsmasq.conf

dnsmasq -C $CONF_DIR/dnsmasq.conf

# Create cert directory
/usr/lib/squid/security_file_certgen -c -s /var/lib/ssl_db -M 16MB

# Copy the SSL certs so that MITM works with squid
mkdir -p /etc/squid/certs
cp $CONF_DIR/ssl-cert/*.pem /etc/squid/certs/

# This will force Vagrant to accept our self signed cert
export SSL_CERT_FILE=/etc/squid/certs/squid-ca-cert.pem

# Create squid swap directories (this does not start proxy yet)
chown proxy:proxy /mnt/squid
squid -f $CONF_DIR/squid.conf -z --foreground

# We then start squid in order to cache the rpm/deb packages
# and handle generic http traffic
squid -f $CONF_DIR/squid.conf

# Set the DNS server point to our address
dns_nameserver=$LOCALIP
dns_search_domain="runner"

cd /mnt/actions-runner

export http_proxy=http://proxy.runner:3128
export https_proxy=http://proxy.runner:3128
export HTTP_PROXY=http://proxy.runner:3128
export HTTPS_PROXY=http://proxy.runner:3128

export RUN_INSIDE_CONTAINER=1

if [ -z "$TESTING" ]; then
    sudo --preserve-env=RUN_INSIDE_CONTAINER,SSL_CERT_FILE,http_proxy,https_proxy,no_proxy,HTTP_PROXY,HTTPS_PROXY,NO_PROXY,containerd_src,crio_src,dns_nameserver,dns_search_domain \
	 -n -u runner ./run.sh &

    wait
else
    bash
fi

if [ $STOPPED -eq 1 ]; then
    exit 1
fi

exit $?
