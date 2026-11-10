# One image, one binary. The same image runs as a single container, as N
# replicas in Kubernetes, or anywhere else with a Postgres to point at.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG DIST=amp
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/amp ./cmd/${DIST}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/amp /amp
EXPOSE 8080
ENTRYPOINT ["/amp"]
CMD ["serve"]
