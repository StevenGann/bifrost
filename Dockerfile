# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bifrost . \
 && mkdir -p /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /bifrost /bifrost
# /data has to exist in the image, owned by the runtime user. Docker creates a
# named volume root-owned when the mount point is absent from the image, and
# then the first key mint fails with EACCES on a compose host.
COPY --from=build --chown=65534:65534 /out/data /data
ENV PORT=11434
EXPOSE 11434
USER 65534:65534
# A scratch image has no curl or wget, so the binary probes itself.
HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/bifrost", "-health"]
VOLUME ["/data"]
ENTRYPOINT ["/bifrost"]
