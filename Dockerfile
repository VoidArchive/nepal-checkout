FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nepal-checkout . && mkdir /out/data

# distroless static ships CA certificates (the gateways are HTTPS) and runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nepal-checkout /nepal-checkout
COPY --from=build --chown=65532:65532 /out/data /data
ENV DB_PATH=/data/checkout.db
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/nepal-checkout"]
