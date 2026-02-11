FROM golang:1.25.6 as builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o pcap-controller main.go

FROM ubuntu:24.04

RUN apt-get update && apt-get install -y \
    tcpdump \
    curl \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV CRICTL_VERSION="v1.30.0"
RUN curl -L "https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VERSION}/crictl-${CRICTL_VERSION}-linux-amd64.tar.gz" --output crictl.tar.gz \
    && tar zxvf crictl.tar.gz -C /usr/local/bin \
    && rm -f crictl.tar.gz

WORKDIR /app

COPY --from=builder /app/pcap-controller /usr/local/bin/pcap-controller

CMD ["/usr/local/bin/pcap-controller"]