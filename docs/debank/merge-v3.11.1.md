# upstream v3.11.1 merge 验证报告

- 日期：2026-07-01
- 升级：`main`(debank，基线 upstream v3.10.2) → **v3.11.1**
- PR：Chaintable/nitro#31（本仓库）+ Chaintable/go-ethereum-arb#25（geth submodule，配套）
- 验证镜像：`public.ecr.aws/b2h7a5c4/chaintable/nitro:amd64-06c4a6c`（PR 镜像，build.yml PR 触发产物）
- 结论：**PASS**（同步区间 blockhash 抽样 6/6 与生产 writer 一致；trace_debankBlock 逐字段一致；节点独立同步至 canonical tip，lag=0）

## 1. upstream 变更摘要（v3.10.2..v3.11.1）

nitro 299 files / geth submodule 21 files。区间跨 v3.11.0-rc.x → v3.11.0 → v3.11.1，核心是 **ArbOS 60/61 支持**正式落地 + point-release 修复：

| 变更 | 影响面评估 |
|---|---|
| ArbOS 60 / ArbOS 61 支持（multi-gas refund、SSTORE refund 记账、SingleDim refund basefee override 等 gas 语义） | **共识执行路径**；本次以逐块 blockhash + trace 实测验证（见 §5），当前区间全程运行于 ArbOS 61 |
| `--execution.transaction-filtering.enable` 取代 `--execution.address-filter.enable`；filtering-report 服务、`FilterSetID` 贯穿 AddressChecker/StateDB 接口 | 配置/接口新增；目标链未启用 filtering，nil-gated no-op |
| `--node.transaction-streamer.shutdown-on-blockhash-mismatch` 默认改 `true` | feed 与本地块 hash 不一致时优雅退出；对只读 backup 是更强的一致性保护 |
| MEL unified-replay binary、`GlobalState` 扩到 4 slot、consensus-v61 replay machine | validation/proving 路径；backup writer `--node.staker.enable=false` 不涉及 |
| Stylus warm-start cache 在 tx drop 时回滚（v3.11.1）；history 读加 mutex（v3.11.1） | 执行正确性修复；随共识路径一并验证 |
| BoLD self-challenge 慢 catchup 降级 fix、redis-validator 初始化 fix | validator 侧，writer 不涉及 |

## 2. 冲突清单与解决依据

两级 merge。**geth submodule 侧近乎无冲突**（唯一冲突 = 一个已废弃 CI workflow 的 modify/delete，取删；debank live-tracer 层与本区间 geth 改动零文本冲突，6 个交集文件全 auto-merge）。**nitro superproject 侧**冲突按来源分两类：

- **CI（11 个 `.github/workflows/*`，modify/delete）**：debank 已迁 `build.yml`/`release.yml`，一律取 ours（删）。
- **content 冲突（~14 个：bold/、cmd/nitro/、execution/gethexec/{addressfilter,node,tx_pre_checker}、util/s3syncer、system_tests、CHANGELOG）**：逐个 `git log` 核实 debank 侧改动**作者全为 upstream**（无 debank-layer commit）——即基线走 v3.10.x release 分支、与 v3.11 主线并行演进产生的**上游自我调和**，非 debank 定制 → 一律取 v3.11.1。
- **Dockerfile**：取 v3.11.1 的 public consensus machine 下载（原 debank 注释针对已消失的 private-repo 拉取形式）+ validator binary；debank 的 builder/ENTRYPOINT 定制经 auto-merge 保留。
- **go.mod/go.sum**：aws-sdk 版本偏移取新；debank 经 submodule 传递的依赖（kafka-go、go.uber）与 upstream 新增（goskiplist、gocovmerge）取并集；`Chaintable/pipeline` 版本与新 geth 一致；go.sum 取两侧 union。
- **go-ethereum submodule 指针** → go-ethereum-arb#25 的 debank merge commit。

**must-preserve 的 debank 面**很小且全部存活：geth 侧 live-tracer（`arbitrum/api_debank.go`、`core/tracing/hooks.go`、`eth/tracers/live/pipeline.go`、statedb hooks）本区间无冲突原样保留；nitro 侧 `arbos/tx_processor.go` tracer patch auto-merge；CI（public ECR / hosted runner / per-chain writer alias）取 ours。

## 3. 双向影响分析

### 3.1 正向（upstream → debank pipeline 采集）

**无影响**。live-tracer 的 hook 点（OnBlockStart/OnTxStart/OnExit/OnBlockEnd）、S3 blockfile/statediff/header 产出、Kafka 上报 upstream 均未触碰；trace_debankBlock（`trace` namespace）on-demand replay RPC 逻辑未变。无新增需下游消费方跟进的字段。

### 3.2 反向（debank patch → 链正确性）

- transaction-filtering / filtering-report 走 estimation / 交易过滤路径，使用 ephemeral state，不经 block import / canonical commit——live-tracer hook 不在该路径触发。
- ArbOS 60/61 的 gas 语义变更属共识执行核心，我方 tracer 为**只读观测**（不改状态），且已由 §5 逐块 blockhash 一致实证不影响执行结果。
- `pushBlockChange`（debank 在 geth `core/blockchain.go` 的 kafka 通知 patch）依赖 pipeline/leader Manager，运行期以 `is_backup=true` 初始化（见 §4），`IsLeader()=false` 跳过 kafka 上报。

### 3.3 build 验证

- geth submodule：`go build ./...` 全量 PASS（本地）。
- nitro：全 workspace 编译依赖 solgen 生成 binding，本地 = 无 solgen 依赖的改动包编译 + gofmt PASS；**完整构建 gate = CI Docker 镜像构建（含 solgen + geth 从源码编译），amd64 + arm64 双架构 PASS**。

## 4. 验证部署方式（内部测试机 backup writer）

| 项 | 值 |
|---|---|
| 镜像 | `nitro:amd64-06c4a6c`（PR 镜像，public ECR） |
| 数据 | 生产 seed writer 当日 EBS 快照恢复（独立卷，by-id 挂载；archive 追块需卷 ≥ 生产 PVC 容量） |
| 参数来源 | 生产 writer sts 实时 spec 照抄，含 `user: "0:0"`（= securityContext.runAsUser，镜像默认 uid 无法打开 root 属主快照数据） |
| **is_backup** | **true**（vmtrace json-config，fixed backup 模式）。**注意：live-tracer 必须启用**——debank 的 `pushBlockChange` 每次写块无条件调 `leader.GlobalManager.IsLeader()`，该 Manager 仅在 pipeline tracer 初始化时创建；去掉 vmtrace 会 nil-panic。backup 模式下 `IsLeader()=false` 跳 kafka 上报；S3 对象键前缀经独立 `version` 隔离，与生产采集互不覆盖 |
| **nodekey** | 数据卷存在 `<chain>/nodekey`，启动前删除。nitro 无 devp2p 同步（启动日志 "P2P server will be useless"），删除属保守措施 |
| parent chain / feed | 与生产一致的端点 |

## 5. 验证判定

| 判定项 | 结果 |
|---|---|
| **同步正确性** | 从恢复快照点独立同步 ~262k 块至 canonical tip，**lag=0**，与生产 writer 同一 head。全程 archive 模式、本 PR 二进制执行，任何执行分叉都会触发 blockhash 不匹配 / fork —— 到达同一 tip 即逐块一致的强证明 |
| **blockhash 抽样** | **6/6 与生产 writer 一致**（同步区间起段 3 + 近 tip 3；均为本二进制执行的 ArbOS 61 块，port-forward 生产 writer 对照） |
| **trace_debankBlock 对账** | 同步区间起段与近 tip 各 1 块，与生产 writer **内容逐字段一致**：`block_file`（indexer 消费的 tx/trace/event 数据）、`header`、`validation_hash` 逐字节相同；唯一差异 = `process_start_timestamp`（wall-clock 元数据）与 `state_diff` 的 RLP 内部序列化顺序（Go map 遍历序，两版本皆非确定，indexer 解码成集合、顺序无关） |
| **trace 自洽** | trace_debankBlock 无 root-mismatch 报错 → StateDiff 收集与 replay 结果同共识一致 |
| **同步健康** | 无 panic / FATAL；`restarts=0`；archive DB 干净打开；chain config 载入 ArbOS 60/61 fork 规则（fork-capable 确认） |

### 说明

- 快照恢复点已在 ArbOS 61 激活块之后，故本次验证覆盖 ArbOS 61 **稳态执行**（~262k 块逐块与生产一致）；激活块本身的一次性 transition 已在生产先行跨越。
- backup 测试节点 S3 采用独立 `version` 前缀隔离，验证结束随现场一并清理；生产采集对象零改动。

**结论：merge 保留全部 debank live-tracer 定制，ArbOS 60/61 执行与生产逐块一致，trace 产出与生产等价，节点独立同步至 canonical tip。PASS。**
