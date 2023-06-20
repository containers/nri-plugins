FROM debian:stable-slim

RUN apt-get update && apt-get install -y software-properties-common build-essential gnupg make curl sudo git socat vim procps

# Install Go 1.19
RUN curl -LO https://golang.org/dl/go1.19.linux-amd64.tar.gz && \
    tar -C /usr/local -xzf go1.19.linux-amd64.tar.gz && \
    rm go1.19.linux-amd64.tar.gz

ENV PATH=$PATH:/usr/local/go/bin
ENV GOPATH=/go
ENV PATH=$GOPATH/bin:$PATH

COPY . .

# Get the Memtierd binary
RUN git clone https://github.com/intel/memtierd.git \
    && cd memtierd/ \
    && make bin/memtierd \
    && mv bin/memtierd /bin \
    && cd ..

RUN GO111MODULE=on go get -u github.com/containerd/nri/pkg/stub
RUN GO111MODULE=on go get -u github.com/sirupsen/logrus
RUN GO111MODULE=on go get -u gopkg.in/yaml.v2

# Build the plugin binary
RUN go build -gcflags "all=-N -l"

RUN mv memtierd-nri /bin/10-memtierd-nri

ENTRYPOINT ["10-memtierd-nri"]
