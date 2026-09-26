FROM golang:alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /teacup .

FROM alpine
RUN adduser -D -H -u 10001 teacup && mkdir -p /app/uploads && chown teacup /app/uploads
WORKDIR /app
COPY --from=build /teacup ./teacup
COPY index.html ./
USER teacup
# .env is mounted at runtime (see docker-compose.yml), never baked into the image.
CMD ["./teacup"]
