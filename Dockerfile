FROM golang:1.25 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o sphinx .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /app/sphinx /sphinx

ENTRYPOINT ["/sphinx"]
