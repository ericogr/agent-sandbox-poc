FROM golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS tools
ENV GOTOOLCHAIN=local GOBIN=/opt/go-tools
RUN go install golang.org/x/tools/gopls@v0.24.0 \
    && go install github.com/go-delve/delve/cmd/dlv@v1.27.2

FROM golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5
ARG TARGETARCH
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) checksum=614877cf71005f14ea252bd2d9b1c12237cf7d73681ef150f0dff7f13849fdb5 ;; \
      arm64) checksum=51481a891754dc2585b86d007aec24ac675453fc5ef6cfa346c4462449277384 ;; \
      *) echo 'A IDE suporta apenas amd64 e arm64.' >&2; exit 1 ;; \
    esac; \
    curl -fSL --proto '=https' --tlsv1.2 "https://github.com/coder/code-server/releases/download/v4.143.1/code-server-4.143.1-linux-$TARGETARCH.tar.gz" -o /tmp/code-server.tar.gz; \
    echo "$checksum  /tmp/code-server.tar.gz" | sha256sum -c -; \
    mkdir -p /opt/code-server; \
    tar -xzf /tmp/code-server.tar.gz --strip-components=1 -C /opt/code-server; \
    ln -s /opt/code-server/bin/code-server /usr/local/bin/code-server; \
    rm /tmp/code-server.tar.gz; \
    useradd --create-home --uid 1000 --shell /bin/bash dev; \
    mkdir -p /opt/code-extensions /etc/code-server; \
    chown dev:dev /opt/code-extensions
COPY --from=tools /opt/go-tools/ /usr/local/bin/
COPY config.yaml settings.json /etc/code-server/
COPY entrypoint.sh /usr/local/bin/dev-entrypoint
RUN chmod 755 /usr/local/bin/dev-entrypoint
ENV HOME=/home/dev GOPATH=/home/dev/go GOTOOLCHAIN=local
ENV PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:/home/dev/go/bin
USER 1000:1000
RUN curl -fSL --proto '=https' --tlsv1.2 https://open-vsx.org/api/golang/Go/0.58.0/file/golang.Go-0.58.0.vsix -o /tmp/go.vsix \
    && echo 'bc638d118041fe91c2b437c7bc478adac9e1b9525490ab39770f4bf10f58a78d  /tmp/go.vsix' | sha256sum -c - \
    && code-server --config /etc/code-server/config.yaml --extensions-dir /opt/code-extensions --install-extension /tmp/go.vsix \
    && rm /tmp/go.vsix
WORKDIR /home/dev
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/dev-entrypoint"]
