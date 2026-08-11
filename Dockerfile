# Two stages, distroless, static, non-root.
#
# ullage never writes to the cluster and never writes to disk except when you
# explicitly ask it to. The image reflects that: no shell, no package manager,
# read-only root filesystem, and a numeric UID so the pod spec can assert
# runAsNonRoot without a lookup.
# Base images are pinned by digest. -trimpath already removes build paths, but
# a floating tag means the same source produces a different binary next week,
# which defeats the point of a reproducible build. Dependabot moves these.
FROM golang:1.25-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/ullage ./cmd/ullage

FROM gcr.io/distroless/static-debian12@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a
COPY --from=build /out/ullage /ullage
USER 65532:65532
ENTRYPOINT ["/ullage"]
CMD ["demo"]
