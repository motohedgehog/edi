# Pin the multi-platform index for the supported amd64 and arm64 builders.
# Update it together with the Go toolchain in a reviewed dependency change.
FROM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations

ARG VERSION=dev
ARG COMMIT=unknown

RUN CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags "-s -w -X github.com/motohedgehog/edi/internal/app.Version=${VERSION} -X github.com/motohedgehog/edi/internal/app.Commit=${COMMIT}" -o /out/edi ./cmd/edi

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/motohedgehog/edi" \
      org.opencontainers.image.description="EDI is a personal Telegram bot." \
      org.opencontainers.image.licenses="GPL-3.0-only"
# nonroot UID/GID
ARG UID=65532
ARG GID=65532
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/edi /edi
ENV EDI_DATA_DIRECTORY=/data
USER ${UID}:${GID}
WORKDIR "${EDI_DATA_DIRECTORY}"
ENTRYPOINT ["/edi"]
