# ---------- сборка ----------
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=docker" -o /out/marina-wind .

# ---------- рантайм ----------
FROM alpine:3.20

# ca-certificates нужны для HTTPS-запросов к Open-Meteo
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app

COPY --from=build /out/marina-wind /usr/local/bin/marina-wind

USER app
ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["marina-wind"]
