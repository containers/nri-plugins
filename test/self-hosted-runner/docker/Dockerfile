ARG BASE_IMAGE
ARG GH_APP_PRIVATE_KEY_FILE

FROM ${BASE_IMAGE} as builder

ARG DEBIAN_FRONTEND=noninteractive
ENV TZ=Etc/UTC

# Install tools
RUN apt-get update -y && apt-get install -y \
  software-properties-common \
  tzdata \
  ansible \
  bash \
  build-essential \
  clang-format \
  git \
  git-lfs \
  jq \
  pv \
  xz-utils \
  bzip2 \
  gzip \
  libffi-dev \
  libssl-dev \
  libbtrfs-dev \
  python3 \
  python3-dev \
  python3-pip \
  python3-venv \
  shellcheck \
  tree \
  wget \
  yamllint \
  zstd \
  apt-transport-https \
  ca-certificates \
  curl \
  gnupg \
  sudo \
  bc \
  lsb-release \
  moreutils

RUN if [ ! -f /usr/share/keyrings/docker-archive-keyring.gpg ]; then \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg; \
  fi && \
  echo "deb [arch=amd64 signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" | tee /etc/apt/sources.list.d/docker.list > /dev/null

RUN add-apt-repository --yes --update ppa:longsleep/golang-backports && \
  echo 'deb [trusted=yes] https://repo.goreleaser.com/apt/ /' | tee /etc/apt/sources.list.d/goreleaser.list && \
  apt-get update -y && \
  apt-get install -y docker-ce docker-ce-cli containerd.io golang goreleaser

RUN apt-get install -y vagrant vagrant-libvirt bridge-utils make \
    libvirt-daemon guestfs-tools qemu-kvm libvirt-daemon-system \
    qemu ebtables libxslt-dev libxml2-dev libvirt-dev zlib1g-dev ruby-dev

COPY runner-in-container.sh /shr/
