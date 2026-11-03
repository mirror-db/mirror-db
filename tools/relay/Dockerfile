FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
# The edge-selection module is a local replace (see go.mod). Its go.mod must
# exist before `go mod download`, or resolution fails on a missing directory.
COPY cft/ ./cft/
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /relay .

FROM alpine:3
RUN apk add --no-cache ca-certificates
COPY --from=build /relay /usr/local/bin/relay
ENTRYPOINT ["relay"]
CMD ["serve"]
