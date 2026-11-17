FROM golang:1.26 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/product-catalog .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/product-catalog /product-catalog
# Numeric, not `nonroot`: Kubernetes runAsNonRoot cannot verify a username.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/product-catalog"]
