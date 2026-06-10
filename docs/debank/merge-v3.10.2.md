# upstream v3.10.2 merge 验证报告

- 日期：2026-06-10
- 升级：v3.10.1 → **v3.10.2**（上次合并基线 v3.10.1）
- PR：Chaintable/nitro#24（本仓库）+ Chaintable/go-ethereum-arb#22（geth submodule，配套）
- 验证镜像：`nitro-node-x:amd64-74fc8eb`（PR 镜像，build.debank.yml PR 触发产物）
- 结论：**PASS**（六项判定全过，新代码同步区间 blockhash 抽样 20/20 与生产 writer 一致）

## 1. upstream 变更摘要（v3.10.1..v3.10.2）

nitro 31 files / geth submodule 2 files，patch release：

| 变更 | 影响面评估 |
|---|---|
| `--execution.transaction-filtering.enable` master switch（默认 false，替代 `--execution.address-filter.enable`） | 配置项新增；目标链未启用 filtering，无行为变化 |
| `EnableETHCallFilter` 收窄到仅 `eth_estimateGas`；prechecker filtering 解耦，sequencer 上跳过 dry-run | API 行为变化（启用 filtering 时才生效）；目标链禁用，nil-gated no-op |
| geth：`gasestimator.Options` 显式 `TxFilterer` 字段，`DoEstimateGas` 显式传入 | 同上，仅 estimation 路径 |
| arbos/l2pricing：MultiGasFees substorage 改 `OpenCachedSubStorage` | **共识执行路径**上的存储 key 哈希推导缓存；changelog 声称语义不变，以实测验证（见 §5） |
| address-filter 新增 `sha256-rawbytesinput` hashing scheme | 未启用 |
| BoLD assertion poster 日志修复、redis-validator 初始化修复 | validator 侧；writer `--node.staker.enable=false` |
| util/s3syncer 改动 | upstream 自家 init/snapshot 同步工具，与 debank S3 pipeline 无关 |

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
- nitro：全 workspace 编译依赖 solgen 生成 binding（需 contracts 工具链），本地验证 = 无 solgen 依赖的改动包编译 + gofmt PASS；**完整构建 gate = CI Docker 镜像构建（含 solgen），PASS**
- go-ethereum-arb 的 `build.debank.yaml` 为 `push: false` 纯编译检查（镜像无消费方，nitro 从 submodule 源码构建），PASS

## 4. 验证部署方式（内部测试机 backup writer）

| 项 | 值 |
|---|---|
| 镜像 | `nitro-node-x:amd64-74fc8eb`（PR 镜像） |
| 数据 | 生产 writer 当天 EBS 快照恢复（独立卷，by-id 挂载） |
| 参数来源 | 生产 writer sts 实时 spec 照抄，含 `user: "0:0"`（= securityContext.runAsUser，镜像默认 uid 1000 无法打开 root 属主快照数据） |
| **is_backup** | **true**（vmtrace json-config 与生产 backup 实例一致）。语义实证：pipeline 模块 pinned tag `tracer/pipeline_tracer.go`（nil=auto/false=leader/true=backup）；**网络层实证：容器 established 连接仅 sequencer feed + parent chain RPC 两条，无 kafka/S3**（"Upload block" 日志为 backup 模式空操作完成日志） |
| **nodekey** | 数据卷存在 `<chain-name>/nodekey`，启动前删除（重新生成）。注：nitro 无 devp2p 同步（启动日志实证 "P2P server will be useless"），删除属保守措施 |
| parent chain / feed | 与生产完全一致的端点 |
| 偏差项 | 无 jrpcx sidecar（RPC 代理，同步验证不需要）；端口/网络独立于机上其他服务 |

## 5. 同步验证数据与结论

- 容器全程 **RestartCount=0**；快照落后约 1h，feed backlog 秒级消化即达 head（catchup 上限 60min 远未触及）

判定六项：

1. **启动日志** ✓ — 无 panic/FATAL/CRIT；WARN 均良性（validator machines 缺失=staker 禁用预期、feed 压缩协商 non-critical、devp2p off）
2. **同步日志** ✓ — 持续稳定导入，无 reorg/peer 异常
3. **eth_blockNumber 增长** ✓ — cron 每分钟采样落 jsonl，观察窗口内持续增长；中途平台期与生产 writer 同高（Arbitrum 系无交易不出块，链安静非节点停滞）
4. **eth_syncing** ✓ — 全程 false（快照恢复后即在 head 附近）
5. **追上 head** ✓ — 与生产 writer（v3.10.1-debank-3）多次对比 lag ≤ 1 块，quiet 期完全同高
6. **blockhash 抽样** ✓ — 新代码同步区间**全量 20 块**逐一与生产 writer 对比，**20/20 一致**——直接验证了 §1 中 l2pricing 共识路径改动"语义不变"的声称

canonical 参照 = 同链生产 writer（同 feed 同数据源的生产可信节点，对 merge 回归为最直接基准）。

## 6. 遗留事项

1. nitro 系 per-chain 部署要点沉淀：nodekey 路径 `<datadir>/<chain-name>/nodekey`；compose 必须含 `user: "0:0"`；本地 build_verify 口径（geth 全量 + nitro 无 solgen 依赖包）
2. 测试现场（卷/目录/容器/采样 cron）验证通过后清理
