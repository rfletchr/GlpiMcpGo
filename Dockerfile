FROM golang:1.26.6 AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o glpi-mcp .

FROM gcr.io/distroless/static-debian12
COPY --from=builder /build/glpi-mcp /glpi-mcp
EXPOSE 3000
ENTRYPOINT ["/glpi-mcp", "-http"]
