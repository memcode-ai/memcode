# memcode gateway — static Go binary on distroless. Build context is the repo
# root:  docker build -f deploy/docker/Dockerfile .
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /memcode-gateway ./gateway/cmd/memcode-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /memcode-gateway /memcode-gateway
ENTRYPOINT ["/memcode-gateway"]
