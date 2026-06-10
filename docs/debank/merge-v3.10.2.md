# hood (Robinhood Chain) — upstream v3.10.2 merge 验证报告

- 日期：2026-06-10
- 链：hood / Robinhood Chain mainnet（chain_id 4663，parent chain = Ethereum mainnet）
- 升级：v3.10.1 → **v3.10.2**（上次合并基线 v3.10.1，metadata 记录一致）
- PR：Chaintable/nitro#24（本仓库）+ Chaintable/go-ethereum-arb#22（geth submodule，配套）
- 验证镜像：`nitro-node-x:amd64-74fc8eb`（PR 镜像，build.debank.yml PR 触发产物）
- 结论：**PASS**（六项判定全过，blockhash 抽样 20/20 与生产一致）

## 1. upstream 变更摘要（v3.10.1..v3.10.2）

nitro 31 files / geth submodule 2 files，patch release：

| 变更 | 影响面评估 |
|---|---|
| `--execution.transaction-filtering.enable` master switch（默认 false，替代 `--execution.address-filter.enable`） | 配置项新增；hood 未启用 filtering，无行为变化 |
| `EnableETHCallFilter` 收窄到仅 `eth_estimateGas`；prechecker filtering 解耦，sequencer 上跳过 dry-run | API 行为变化（启用 filtering 时才生效）；hood 禁用，nil-gated no-op |
| geth：`gasestimator.Options` 显式 `TxFilterer` 字段，`DoEstimateGas` 显式传入 | 同上，仅 estimation 路径 |
| arbos/l2pricing：MultiGasFees substorage 改 `OpenCachedSubStorage` | 存储 key 哈希推导缓存，纯性能优化，语义不变 |
| address-filter 新增 `sha256-rawbytesinput` hashing scheme | hood 未启用 |
| BoLD assertion poster 日志修复、redis-validator 初始化修复 | validator 侧；hood `--node.staker.enable=false` |
| util/s3syncer 改动 | upstream 自家 init/snapshot 同步工具，与 debank S3 pipeline 无关 |

Step 1 判定"涉及"（API 正确性面 + ArbOS 状态读路径），走完整 merge+验证流程。

## 2. 冲突清单与解决依据

两级 merge，**唯一冲突 = `go-ethereum` submodule 指针**（双方都动了它）：

- upstream：`ad96ae46b` → `6be44be66`
- 我方 debank：`ad96ae46b` 基础上有 `3047f903b`（pushBlockChange nil-check fix）
- **解决**：go-ethereum-arb 单独走 merge（`merge-6be44be6-for-v3.10.2` 分支，PR #22，零冲突自动合并），nitro 侧 submodule 指向其 merge commit `937af5a98a`

文件级冲突：**零**。我方 patch 面（nitro：arbos/tx_processor.go OnExit gasUsed 置 0、Dockerfile private-repo 认证、workflows、go.mod；geth：pipeline live tracer、statedb/stateupdate/blockchain hooks、api_debank）与 upstream 改动文件零交集（comm -12 实测）。

## 3. 双向影响分析

### 3.1 正向（upstream → debank pipeline 采集）

**无影响**。S3 statediff/blockfile/header 产出、Kafka 上报、vmtrace hook 点（OnBlockStart/OnTxStart/OnExit/OnCommit 等）upstream 均未触碰。无新增需要下游 pipeline 消费方跟进的字段。

### 3.2 反向（debank patch → 链本身正确性）

- upstream 新代码路径（TxFilterer 注入、prechecker 解耦）走 eth_estimateGas/交易过滤路径，使用 ephemeral state，不经过 block import / canonical commit——我方 hooks 不在该路径触发，无交互
- l2pricing 缓存改动在 ArbOS 状态读，与我方 arbos/tx_processor.go patch（StartTxHook 内 fake-call OnExit）无重叠
- `NewTxPreChecker` 签名变化：我方无调用方，无适配需要

### 3.3 build 验证

- geth submodule：`go build ./...` 全量 PASS（本地）
- nitro：全 workspace 编译依赖 solgen 生成 binding（需 contracts 工具链），本地验证 = 无 solgen 依赖的改动包编译 + gofmt PASS；**完整构建 gate = CI Docker 镜像构建（含 solgen），PASS**（PR build run 27266547760）
- go-ethereum-arb 的 `build.debank.yaml` 为 `push: false` 纯编译检查（镜像无消费方，nitro 从 submodule 源码构建），PASS

## 4. 部署参数（backup writer @ lihe-dev）

| 项 | 值 |
|---|---|
| 镜像 | `nitro-node-x:amd64-74fc8eb`（PR 镜像） |
| 快照 | `snap-02ac6a51929f1d0a3`（writer=nodex-node-seed-0，2026-06-10T08:39Z，10GiB，周频，当天新鲜） |
| 卷 | `vol-0c7c10d56644d678f`（gp3 3000/125，tag chain=hood/type=writer-backup/run=merge-v3.10.2），/dev/sdl，by-id 挂载 |
| 路径 | `/opt/app/hood/writer_merge_v3.10.2/`（compose.yml + configs/ + data/） |
| 参数来源 | chaintables 集群 `blockchain-hood/nodex-node-seed` sts 实时 spec 照抄（含 `user: "0:0"` = securityContext.runAsUser，首启缺它 LOCK permission denied） |
| **is_backup** | **true**（VMTRACE_JSONCONFIG 与生产 seed 完全一致）。语义实证：pipeline v0.0.61-nitro-v3.5.6-2 `tracer/pipeline_tracer.go:44`（nil=auto/false=leader/true=backup）；**网络层实证：容器 established 连接仅 feed(443)+parent-chain(80)，无 kafka:9092/S3**（"Upload block" 日志为 backup 模式空操作完成日志） |
| **nodekey** | 数据卷存在 `Robinhood Chain/nodekey`，启动前已删除（重新生成）。注：nitro 无 devp2p 同步（启动日志实证 "P2P server will be useless"），删除属保守措施 |
| parent chain | `trace.eth.blockchain` / `archive-beacon.eth.blockchain`（VPC 私有 DNS，与生产完全一致） |
| 偏差项 | 无 jrpcx sidecar（RPC 代理，同步验证不需要）；端口 127.0.0.1:28545/29260（18545 被既有 hood-leader-test 占用）；独立 network 10.46.63.0/24 |

## 5. 同步验证数据与结论

- 容器启动：2026-06-10T09:35:54Z，全程 **RestartCount=0**
- 追块：快照落后约 1h，feed backlog 秒级消化，~09:36 即达 head（`catchup_timeout` 60min 远未触及）

判定六项：

1. **启动日志** ✓ — 无 panic/FATAL/CRIT；WARN 均良性（validator machines 缺失=staker 禁用预期、feed 压缩协商 non-critical、devp2p off）
2. **同步日志** ✓ — 持续稳定导入（Imported new chain segment / created block），无 reorg/peer 异常
3. **eth_blockNumber 增长** ✓ — cron 每分钟采样（samples.jsonl，9 点 / 09:40–09:48）：47157 → 47168 持续增长；中途数分钟平台期与 prod writer 同高（Arbitrum 系无交易不出块，链安静非节点停滞）
4. **eth_syncing** ✓ — 全程 false（快照恢复后即在 head 附近）
5. **追上 head** ✓ — 与 prod writer（nodex-node-cd015ae1-0，v3.10.1-debank-3）多次对比 lag ≤ 1 块，quiet 期完全同高
6. **blockhash 抽样** ✓ — 新代码同步区间全量 20 块（47143–47162），本地 vs prod writer 逐块对比 **20/20 一致**：

| 块号 | hash（双方一致） |
|---|---|
| 47143 | 0x786e368878b3e22389c3cde9859003d1aa07fc2cc967be38a545771a9aa00c18 |
| 47144 | 0xbb87f28bbc4160bf6013cddbea290584a85e4a553c9fa163d3b5dcb3a423dfff |
| 47145 | 0x1dcd48f73e8ac922312bd89c33e61db221511588b8228c6843d4ef8ef8a9a0e1 |
| 47146 | 0xc80ad1bb58ae66013b7db7d18f857c42672aaca81cb9303978c6df19a2ed8b9a |
| 47147 | 0xf1b84c7a9bb7c98ca585e4862395dfec9578d744ec39851d614851be39d8731a |
| 47148 | 0x930faf6ed0776b7a6cf54110332ae97834b37b13bd753fb2cb7230706fa8b0d7 |
| 47149 | 0x214f31a8fd7da94d40460020a4bb9b37a316f95e9d39519b2bec86d37c545de8 |
| 47150 | 0x8ad79a0d7f7cef9e749dcdc480615900a25d500b294425820aa55b83b36218fe |
| 47151 | 0x26d358f3527e9089350d77df7f59d3f98c49e7e785f5f7679e8387d28bd55f2f |
| 47152 | 0x375e8b7e002bacaf49694b92310e53d452aac08689d4a1f6026bc0aae6aa1501 |
| 47153 | 0x1016fac206004e0d392cc3a9c31d0d22d32661d5c3e08c7ff196fcc53636bf92 |
| 47154 | 0x7c033f25df0a8f610ad3d20dded1ea933b3fb83adcf72dc625cba1cc366d1e46 |
| 47155 | 0x196478c9eee67c4a69fdd76f7340df391d8e48fcf2ef1c1565855fbdf3d85ab0 |
| 47156 | 0xc4423931d04b3be286c50da3828b04acc9d1557873d9f3dde62b476f1656e73f |
| 47157 | 0x4f6285a7f2ea2275631f0a808070575b92f1f44470b1ddeaefd929ad28bfaba6 |
| 47158 | 0x0eaab09d6493213383bafe071140e554290b0fe9b67cc349a2a5877fffa1e041 |
| 47159 | 0xf7277e01f6025956540aa93d5df80cbffaa3f2d19f634c7d1bc1feffa711650b |
| 47160 | 0x206dd521af28680bcce2c258ccec3ae945400e5266d33f0dbd4ef65cf0b6943a |
| 47161 | 0xc3a6e54f886a2e0c5a6889003214b5d10be69a6d911ce121f0e9afdf9a487c36 |
| 47162 | 0xcdd8f21fa5943c7b1d5cb9c22f4c936c3d41f4ca693e425a40025afc31476d02 |

### 对比参照说明（重要）

metadata 记录的 `official_rpc`（rpc.testnet.chain.robinhood.com）是 **testnet（chainId 46630）**，与本链（mainnet 4663）不是同一网络；mainnet 未注册 chainlist、无公开 RPC。本验证以 **prod writer** 为 canonical 参照（同 feed 同链的生产可信节点，对 merge 回归检查为最直接基准）。

## 6. 遗留事项

1. **metadata official_rpc 待修正**：hood entry 的 official_rpc 应换成 mainnet 端点（待 Robinhood 公布公开 RPC，或暂记内部端点）——metadata 回写 PR 时一并处理或单独跟进
2. nitro 系 per-chain 配置沉淀：nodekey_paths=`<datadir>/<chain-name>/nodekey`；compose 必须含 `user: "0:0"`；本地 build_verify 口径（geth 全量 + nitro 无 solgen 包）
3. 测试现场清理（compose down / umount / detach / 删卷 / 删目录 / 删分支 / 移除采样 cron）待用户确认后执行（Step 9）
