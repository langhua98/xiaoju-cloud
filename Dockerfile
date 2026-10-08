FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/xiaoju ./cmd/xiaoju

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/xiaoju /usr/local/bin/xiaoju
WORKDIR /app
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["xiaoju"]
CMD ["serve"]
