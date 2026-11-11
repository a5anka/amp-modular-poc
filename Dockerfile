# Two images from one build:
#   docker build -t amp .                                  core binary (default target)
#   docker build --target eval-worker -t amp-eval-worker .  evaluation workloads (needs python3 for custom evaluators)
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG DIST=amp
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/amp ./cmd/${DIST} && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/amp-eval-worker ./cmd/amp-eval-worker

# eval-controller, shared-worker and the isolated-run launcher share this image;
# the module version pins its tag/digest.
FROM python:3.13-slim AS eval-worker
RUN useradd --uid 10001 --no-create-home amp
COPY --from=build /out/amp-eval-worker /usr/local/bin/amp-eval-worker
USER 10001
ENTRYPOINT ["amp-eval-worker"]

FROM gcr.io/distroless/static-debian12:nonroot AS core
COPY --from=build /out/amp /amp
EXPOSE 8080
ENTRYPOINT ["/amp"]
CMD ["serve"]
