PREFIX ?= `pwd`/shr

BASE_IMAGE_BUILD_CMD ?= docker build
BASE_IMAGE_BUILD_EXTRA_OPTS ?=
BASE_IMAGE ?= ubuntu:22.04

VM_BASE_IMAGE ?= https://vagrantcloud.com/generic/boxes/fedora37/versions/4.2.14/providers/libvirt.box
VM_BASE_IMAGE_FILENAME ?= generic-fedora37-4.2.14.box
VM_DIR ?= $(PREFIX)/mnt/vagrant

# Note that the env file needs to be created by the user either
# manually or by "make create-env" command.
all: provision env

provision: base-image download-vm

base-image: base-image-done

base-image-done: .base-image.done.stamp
.base-image.done.stamp: docker/Dockerfile docker/runner-in-container.sh
	$(BASE_IMAGE_BUILD_CMD) \
	    --build-arg BASE_IMAGE=$(BASE_IMAGE) \
	    -t shr-base-image \
	    -f docker/Dockerfile docker
	$(Q)if [ ! -f "$@" ]; then \
	    touch "$@"; \
	fi;

create-env: env.in
	$(Q)cp -i env.in env

cleanup:
	$(Q)rm -f .*.stamp

run: base-image-done
	PREFIX=$(PREFIX) ./runner.sh

download-vm: .vm-image.done.stamp
.vm-image.done.stamp:
	$(Q)mkdir -p $(VM_DIR) && \
	curl --progress-bar -L $(VM_BASE_IMAGE) > $(VM_DIR)/$(VM_BASE_IMAGE_FILENAME) && \
	if [ ! -f "$@" ]; then \
	    touch "$@"; \
	fi
