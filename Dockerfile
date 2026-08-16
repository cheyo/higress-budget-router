# Higress Wasm 插件 OCI 镜像：只需把 plugin.wasm 放在根路径
FROM scratch
COPY main.wasm plugin.wasm
