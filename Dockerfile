# Builder phase.
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -a -o k8s-dummy-device-plugin dummy.go

# Runtime phase.
FROM alpine:3.22
COPY --from=builder /src/k8s-dummy-device-plugin /k8s-dummy-device-plugin
COPY --from=builder /src/dummyResources.json /dummyResources.json
ENTRYPOINT ["/k8s-dummy-device-plugin"]
