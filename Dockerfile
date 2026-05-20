FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/p2pcall .

FROM alpine:3.22

WORKDIR /app

RUN adduser -D -H -u 10001 appuser

COPY --from=build /out/p2pcall /app/p2pcall
COPY static /app/static

USER appuser

EXPOSE 3000

CMD ["/app/p2pcall"]
