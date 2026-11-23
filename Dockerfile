FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/glimpse ./cmd/server
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/glimpse /glimpse
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV HOST=0.0.0.0 DB_PATH=/data/glimpse.db
VOLUME /data
EXPOSE 4318
USER nonroot
ENTRYPOINT ["/glimpse"]
