FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/panda-api ./cmd/api \
 && mkdir -p /out/uploads

# static:nonroot sudah memuat CA certificates (HTTPS ke LLM/reranker/MinIO) dan tzdata.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/panda-api /app/panda-api
# Volume uploads baru mewarisi pemilik nonroot dari folder ini.
COPY --from=build --chown=nonroot:nonroot /out/uploads /app/uploads
EXPOSE 8080
CMD ["/app/panda-api"]
