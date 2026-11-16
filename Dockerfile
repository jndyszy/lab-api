# lab-api 镜像：多阶段构建，最终镜像只有一个静态二进制
# ---- 构建阶段 ----
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
# 先只复制依赖清单，依赖不变时这一层可复用缓存
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/lab-api .

# ---- 运行阶段 ----
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/lab-api /lab-api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/lab-api"]
