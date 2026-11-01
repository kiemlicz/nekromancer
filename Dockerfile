FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/ ./cmd/...

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/controller /out/node-agent /
ENTRYPOINT ["/controller"]
