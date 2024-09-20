FROM --platform=linux/amd64 golang:1.22.2-alpine3.19 AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM
ARG BUILDPLATFORM
ARG CGO_ENABLED=1

WORKDIR /workspace

# Copy source code
COPY . .

# Go get dependencies
RUN go mod download
RUN go mod tidy
# # Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -a -o keptn-server main.go

# # Use distroless as minimal base image to package the manager binary
# # Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot as develop
WORKDIR /
COPY --from=builder /workspace/keptn-server .
USER 65534:65534

CMD ["./keptn-server"]
