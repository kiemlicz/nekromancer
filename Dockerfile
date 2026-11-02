# Cross-compiles on the build host, so no emulation is needed for other platforms.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/ ./cmd/...

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/controller /out/node-agent /
ENTRYPOINT ["/controller"]
