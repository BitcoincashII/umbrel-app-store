FROM ubuntu:24.04

ARG BCH2_VERSION=27.0.0

RUN apt-get update && apt-get install -y \
    ca-certificates \
    curl \
    libminiupnpc17 \
    libnatpmp1 \
    libevent-pthreads-2.1-7 \
    libevent-2.1-7 \
    libzmq5 \
    libsqlite3-0 \
    && rm -rf /var/lib/apt/lists/*

# Download and extract BCH2 binaries from release
RUN curl -L https://github.com/BitcoincashII/bitcoincashII-core/releases/download/v${BCH2_VERSION}/bitcoincashII-${BCH2_VERSION}-linux64.tar.gz | \
    tar -xz -C /tmp && \
    cp /tmp/bitcoincashII-${BCH2_VERSION}-linux64/bin/bitcoincashIId /usr/local/bin/ && \
    cp /tmp/bitcoincashII-${BCH2_VERSION}-linux64/bin/bitcoincashII-cli /usr/local/bin/ && \
    rm -rf /tmp/bitcoincashII-* && \
    chmod +x /usr/local/bin/bitcoincashII*

RUN mkdir -p /root/.bch2

EXPOSE 8332 8333

ENTRYPOINT ["bitcoincashIId"]
CMD ["-printtoconsole", "-prune=1000"]
