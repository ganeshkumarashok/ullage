# Two stages, distroless, static, non-root.
#
# ullage never writes to the cluster and never writes to disk except when you
# explicitly ask it to. The image reflects that: no shell, no package manager,
# read-only root filesystem, and a numeric UID so the pod spec can assert
# runAsNonRoot without a lookup.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/ullage ./cmd/ullage

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ullage /ullage
USER 65532:65532
ENTRYPOINT ["/ullage"]
CMD ["demo"]
