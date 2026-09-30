#!/bin/bash

# 产物安装到用户级软件目录（~/.local/bin，已在 PATH 中），可直接运行。
bin_dir="${HOME}/.local/bin"
mkdir -p "${bin_dir}"

go build -o "${bin_dir}/aiusage" ./cmd/gateway
