# APT / Linux 软件包发布预留说明

`.goreleaser.yaml` 已通过 `nfpms` 自动构建并随 GitHub Release 附带以下原生 Linux 安装包（含 `/usr/lib/systemd/user/wecom-push.service`）：

- `.deb`（Debian / Ubuntu，`amd64` & `arm64`）
- `.rpm`（RHEL / Fedora / CentOS / Rocky，`amd64` & `arm64`）
- `.apk`（Alpine Linux，`amd64` & `arm64`）

## 当前手动安装 `.deb`

```sh
curl -LO https://github.com/Cinea4678/wecom-push/releases/download/v0.1.0/wecom-push_0.1.0_linux_amd64.deb
sudo apt install ./wecom-push_0.1.0_linux_amd64.deb
```

## 未来接入 APT 仓库的扩展路径

当希望支持 `apt update && apt install wecom-push` 时，可直接复用 `nfpms` 产出的 `.deb` 包，任选其一即可：

1. **GitHub Pages 静态 APT 仓库**（零成本）：
   - 在 `.github/workflows/release.yml` 的 GoReleaser 步骤后增加一个 job，用 `dpkg-scanpackages` + GPG 签名生成 `Packages.gz` / `InRelease` 并推送到 `gh-pages` 分支或独立 `apt-repo` 仓库。
2. **Cloudsmith / Fury 托管源**（免维护签名）：
   - 在 `.goreleaser.yaml` 取消注释 `publishers` 块或添加 `fury` / `cloudsmith` 上传步骤，在 GitHub Secrets 中填入对应 API Token 即可自动推送。
