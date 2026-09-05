# Контекст сборки — каталог mail: docker build -f cmd/sesfake/Dockerfile .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sesfake ./cmd/sesfake

# distroless/static:nonroot — ни shell, ни пакетного менеджера, uid 65532.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/sesfake /sesfake
EXPOSE 8080
ENTRYPOINT ["/sesfake"]
