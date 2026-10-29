# GitHub Actions 构建与发行

仓库内 Build and release 工作流在推送 v 开头的版本标签后执行，支持 v1.0.0、v1.0.0-rc.1 格式。版本号写入程序 version，Git revision 写入 commit。普通分支推送/PR只运行 Go CI，不发布；无需给 NAS 安装编译工具，也无需个人访问令牌或额外部署密钥。

## 发行步骤

先将代码提交推送到 master，确认普通 CI 通过，再打标签：

```sh
git tag -a v1.0.0 -m "v1.0.0"
git push origin v1.0.0
```

推送版本标签代表发布指令。工作流解析标签对应的确切 commit，运行依赖校验、vet 和带 PostgreSQL 的完整 race 测试，再交叉构建。测试失败不会进入发行阶段。预发行标签含 - 后缀时标记为 prerelease。

手动触发：GitHub 仓库 Actions → Build and release → Run workflow，填写已经存在的版本标签。默认 Keep the release as a draft 为勾选状态；取消勾选才正式发布。手动模式也会测试和构建标签对应的 commit，不会误用选中分支的代码。首次需要将本工作流推送到默认分支，GitHub 才显示手动运行入口。

## 发行资产

- cyapi-nas-linux-amd64.tar.gz：Intel/AMD NAS，解压后 cyapi + INSTALL.md。
- cyapi-nas-linux-arm64.tar.gz：ARM64 NAS，相同目录结构。
- cyapi-windows-amd64.zip：Windows 程序与使用说明，不含自动 Windows 服务注册。
- cyapi-linux-amd64、cyapi-linux-arm64、cyapi-windows-amd64.exe：独立程序，适合升级命令使用。
- SHA256SUMS：上述六个文件的校验值。

每个发行包只显式包含程序与安装说明，配置、密钥、数据库、测试账号、音源和本机日志均不打包。压缩包使用 commit 时间作为时间戳；同一输入重复打包保持一致。release.sh/release.ps1 也生成相同资产，打包辅助程序只使用 Python 标准库（构建环境需要 Python 3.11+，NAS 运行不需要）。不要混用不同版本的程序和校验文件。

NAS 示例：

```sh
sha256sum -c SHA256SUMS --ignore-missing
mkdir -p /vol1/appdata/cyapi
# 将对应架构的安装包解压到该目录，再执行：
cd /vol1/appdata/cyapi
sudo ./cyapi install --public-url https://api.example.com
```

升级下载独立 Linux 程序，改名 cyapi-new，按 SHA256SUMS 中该程序的摘要执行 upgrade --file ./cyapi-new --sha256 摘要。首次上线仍须在 NAS 验证 systemd、权限、frp 和恢复流程。

## 发布与权限策略

工作流默认仅 contents:read，只有上传发行资产的 publish job 使用 contents:write；依赖内置 GITHUB_TOKEN，不需要配置 PAT。测试/构建 job 不具备发行写权限。若组织策略禁止 GitHub Actions 创建 Release，需仓库管理员调整策略；不要将个人 token 写入仓库作为绕过。

资产先上传到草稿，全部上传成功才转为正式 Release，避免公开半套文件。相同标签并发运行串行处理；失败后可重跑；对于已存在的草稿或正式 Release，逐个核对已有文件 SHA256，仅补齐缺失文件，相同文件跳过，不同内容明确报错且不覆盖。支持先在网页创建只有源码的 Release，再由工作流补齐程序。已有正式 Release 的标题、说明和公开状态保持不变。发布前再次确认标签仍指向构建时的 commit。不要移动已发布版本标签，应发布新版本。

本地构建脚本会将未提交工作树标记为 +working。CI 从标签对应的干净 commit 构建，不附加该标记。发行不启用自动下载更新、Docker 镜像发布或自动修改 NAS 服务。
