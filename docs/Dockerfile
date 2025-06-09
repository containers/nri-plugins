FROM sphinxdoc/sphinx:7.4.7

ARG GO_VERSION=1.24

RUN apt-get update && apt-get install -y wget git

# Note: Any golang version that can 'go list -m -f {{.Variable}}' is fine...
RUN wget https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz && \
    tar -C /usr/local -xzf go${GO_VERSION}.linux-amd64.tar.gz

ENV PATH=$PATH:/usr/local/go/bin

COPY requirements.txt .

RUN pip3 install -r requirements.txt
