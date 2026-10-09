# syntax=docker/dockerfile:1
# CI builds from a mirror of the same image (GO_IMAGE) so a Docker Hub rate limit cannot fail a build.
ARG GO_IMAGE=golang:1.25-alpine
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signallab ./cmd/signallab

# Static binary on a minimal non-root image. The binary doubles as its own
# health probe (`signallab healthcheck`) because the image has no shell or curl.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/signallab /signallab
EXPOSE 8080
ENTRYPOINT ["/signallab"]
CMD ["serve"]
