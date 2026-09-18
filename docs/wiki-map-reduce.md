# Wiki 管线 map-reduce 改造：超长文档全量覆盖

> 本分支（`wiki-map-reduce`）是 [Tencent/WeKnora](https://github.com/Tencent/WeKnora) 的本地魔改版。核心改动：wiki 知识图谱管线从「全文截断」升级为「map-reduce 分段处理」，538 万字符的长文档可以全量进入 LLM 流程，字符上限可调，中英文书籍自适应。

## 1. 解决什么问题

原版 `mapOneDocument` 对文档全文做硬截断（`maxContentForWiki = 32768` 字符），超出部分在 pass 0 实体抽取、summary 生成时**直接丢失**——一本 538 万字符的英文书，LLM 只能看到前 3.3%。chunk 切分和向量化本身没有问题，坏在 summary/postprocess 的 LLM 消费端。

对全文消费点的精确分析（魔改前的实测结论）：

| 消费点 | 截断影响 | 处理方式 |
|--------|----------|----------|
| pass 0 实体抽取（extractCandidateSlugs） | **受影响** | 多段并发，每段全量 |
| summary（WikiSummaryPrompt） | **受影响** | map-reduce（分段摘要 + reduce 合并） |
| retractStale（RetractDocContent） | 受影响 | 保持截断版（设计定案：撤回场景只需近期上下文） |
| classifyChunkCitations | 免疫（天然按 chunk 分批） | 零改动 |
| reduceSlugUpdates（editor） | 免疫（只吃 DocSummary + 被引 chunk） | 零改动 |

## 2. 核心方案

```
原文档 (raw chars)
  │
  ├─ raw <= effective_max ---> 原单段路径（零语义改动，行为完全兼容）
  │
  └─ raw > effective_max ---> planSegmentation 切段（chunk 边界累加 + ±10% 窗口 H1 行首对齐 + 零丢失断言）
        │
        ├─ pass 0: extractCandidateSlugsMultiSegment（并发 3 段 → per-slot 收集 → 段序合并 →
        │          同 slug 合并（alias 并集/描述保长/冲突保首现）→ LLM 级去重兜底 →
        │          失败 fallback 吃截断版护住 legacy extractor）
        ├─ summary: generateSummaryMultiSegment（每段独立 WikiSummaryPrompt 调用，
        │          成功数 >= 一半才进入 reduce；WikiSummaryReducePrompt 输出契约与单段一致，
        │          跨段去重、保留段独有主题、防幻觉）
        └─ classify / editor / cross-links: 零改动
```

**切段器关键设计**（`wiki_segment.go`）：
- 以 chunk 为最小单位累加，超上限后向前找最近的 H1 行做段尾对齐，允许 ±10% 漂移——段边界落在章节边界而不是硬切
- 零丢失：round-trip 断言 `joinSegments(segs) == joinedRoundTrip(chunks)`（测试内置）
- 段头自动注入书名/段序/字符范围，让每段 LLM 调用知道自己处理的是全书的哪一部分

## 3. 可调参数（系统设置，改完即时生效，无需重启）

| 参数 | 环境变量 | 默认 | 说明 |
|------|----------|------|------|
| `wiki.segment_max_chars` | `WEKNORA_WIKI_SEGMENT_MAX_CHARS` | 200000 | 单段字符上限（>=10000） |
| `wiki.segment_token_budget` | `WEKNORA_WIKI_SEGMENT_TOKEN_BUDGET` | 0（关闭） | >0 时按内容密度自动换算字符上限 |

**token 预算模式（推荐用于中英文混部）**：`budget > 0` 时按 CJK 占比 `p` 计算密度 `0.65p + 0.25(1-p)`，换算 `上限 = clamp(budget / 密度, 50000, 800000)`。效果：中文书自动约 20 万字符/段，英文书自动约 52 万字符/段，一个参数适配两种语言。

参数走系统设置注册表（DB > 环境变量 > 默认值），运行时实时读取，**改参数不需要重启服务**。

## 4. 提交清单

| commit | 内容 |
|--------|------|
| `54ce377` | 系统设置注册 2 个参数条目 + key 级校验 |
| `f6761d8` | 内容切段器（H1 对齐 + 零丢失）16 测试 48 断言 |
| `ab57e0a` | 多段 pass 0 候选抽取 + 跨段合并（8 测试函数 30+ 断言） |
| `356a887` | summary map-reduce + reduce 提示词契约（5 测试函数 20+ 断言） |
| `cd1bbb2` | mapOneDocument 集成：planSegmentation 决策 + 单段等价性（6 测试 31 断言） |
| `f5efea6` | 部署级补丁（见下节） |

## 5. 端到端验证（60 万字符英文模拟书，2026-09-18）

- 触发：`raw=599,841 > 200,000 → segments=3`，mode=manual
- H1 对齐生效：段边界 209,977 / 419,965（±10% 窗口对齐到章边界，非硬切）
- summary map-reduce：reduce 页明确覆盖三段全部字符范围，最后一章（42-60 万字符区）的实体表格完整列出
- **黄金判据**：最后一个章节的实体（旧逻辑下必被截断丢弃）成功独立建页且正确归属章节
- 23 页生成 + 14 页交叉链接注入；防幻觉与 `[[slug|name]]` 双链规则正常

## 6. 部署级补丁（`f5efea6`，与 map-reduce 无关但随仓库携带）

- `docker-compose.yml`：DeepSeek API 的容器内 DNS 修复（extra_hosts 指向可达 CDN 节点，云机内部 DNS 解析不可用时的 workaround）
- `docker/Dockerfile.app`：rustup/cargo 换 rsproxy.cn 国内镜像（官方源在本环境下载 35 分钟不止步，镜像源 19 秒完成）
- `internal/models/vlm/remote_api.go`：GLM-4V-Flash 免费档 max_tokens 收敛到 1024
- `internal/infrastructure/docparser/mineru_cloud_converter.go`：MinerU Cloud API 引擎接入
- 其余为 markdown 解析健壮性小修

## 7. 回滚与兼容

- 单段路径零语义改动：`raw <= effective_max` 时七个消费点取值与原版完全一致（含日志与 span 属性）
- 不启用新参数（保持默认 200000）时，原 32768 截断行为的差异仅体现在 retractStale 上下文变长
- 任何阶段异常可回退到官方 main；系统设置参数随时可改回
