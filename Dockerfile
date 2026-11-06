# Pin the multi-platform index for the supported amd64 and arm64 builders.
# Update it together with the Go toolchain in a reviewed dependency change.
FROM golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS build

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
