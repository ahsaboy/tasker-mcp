# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS builder
WORKDIR /src

# Cache module downloads.
COPY cli/go.mod cli/go.sum ./cli/
RUN cd cli && go mod download

COPY cli/ ./cli/

ARG VERSION=dev
RUN cd cli && \
    CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/tasker-mcp ./...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/tasker-mcp /usr/local/bin/tasker-mcp
COPY dist/toolDescriptions.json /etc/tasker-mcp/toolDescriptions.json
USER nonroot
EXPOSE 8000
ENTRYPOINT ["/usr/local/bin/tasker-mcp"]
CMD ["--tools", "/etc/tasker-mcp/toolDescriptions.json", "--host", "0.0.0.0", "--port", "8000"]
