# 图像模型（gpt-image-2 系列）接入笔记

记录通过 apiproxy 代理阿里云/bella 图像模型时观察到的**端点、响应结构、计费口径**，以及代理当前的处理方式和计费待办。数据来自实际抓包（observe 模式日志）。

> 状态：**仅代理 + 观察，尚未计费**。两个模型通过 `OBSERVE_MODELS` 放行并落库（费用 ¥0）。

## 端点与请求格式

| 模型 | 端点 | 请求体 | 说明 |
|------|------|--------|------|
| `gpt-image-2` | `POST /v1/images/generations` | JSON `{"model":"gpt-image-2", ...}` | 文生图 |
| `gpt-image-2-edits` | `POST /v1/images/edits` | **multipart/form-data**（字段 `model` / `prompt` / `image=@文件`） | 图生图/编辑 |

要点：
- **编辑走 multipart**，模型名在表单字段里，不在 JSON。代理用 `extractMultipartModel`（`internal/proxy/proxy.go`）从 multipart 里取 `model`，否则观察/计费逻辑（以 `model != ""` 为前提）会被整体跳过。
- Codex 默认走 **Responses API**（`/v1/responses`）。若上游某渠道/key 无该端点权限会报 `没有支持当前endpoint的可用渠道`。让 Codex 改用 Chat Completions（`wire_api = "chat"`）或换有权限的 key 即可——**与 apiproxy 无关**。

## 响应结构（两个模型一致）

```json
{
  "background": "...",
  "created": 1752... ,
  "data": [ { "b64_json": "<base64 图像>" } ],     // data 数组长度 = 出图张数
  "usage": {
    "num": 1,
    "size": "1024x1024",
    "quality": "low" | "high",
    "input_tokens": 152,
    "input_tokens_details": { "text_tokens": 152, "image_tokens": 0 },
    "output_tokens": 196,
    "total_tokens": 348
  }
}
```

- 响应**体积大且经 gzip 压缩**（`data` 里是 base64 图像）。代理转发原始压缩字节给客户端，但**解析 usage / 打观察日志前会先解压一份副本**（`maybeGunzip`），否则解析出来全是 0。
- `input_tokens = text_tokens + image_tokens`。生成时 `image_tokens=0`；编辑时 `image_tokens` 为输入图的 token。

### 实测样本

| 场景 | quality | text_tokens | image_tokens | output_tokens | num |
|------|---------|-------------|--------------|---------------|-----|
| 生成 gpt-image-2 | low | 152 | 0 | 196 | 1 |
| 编辑 gpt-image-2-edits | high | 126 | 1024 | 229 | 1 |

## 计费口径（来自定价卡）

| 模型 | 定价 |
|------|------|
| `gpt-image-2` | 文本 **35** / 输入图 **56** / 输出图 **210**（元 / 百万 token） |
| `gpt-image-2-edits` | **1.5 元/张** + 图片 token **210** 元/百万 token |

### gpt-image-2 字段 → 单价映射（较明确）

| 定价项 | usage 字段 | 单价 |
|--------|-----------|------|
| 文本 | `input_tokens_details.text_tokens` | 35/M |
| 输入图 | `input_tokens_details.image_tokens` | 56/M |
| 输出图 | `output_tokens` | 210/M |

示例（生成样本）：`152×35 + 0×56 + 196×210 = 46480 厘/百万 = 0.0465 元`。

### gpt-image-2-edits 的**待确认矛盾** ⚠️

定价卡写"1.5 元/张 + token"，但响应体给的是**和生成一样的 token 明细**。同一次编辑两种算法差 10 倍以上：

| 算法 | 编辑样本费用 |
|------|-------------|
| 按 token（文本35/输入图56/输出图210，同生成） | `126×35 + 1024×56 + 229×210 = 0.110 元` |
| 按张（1.5/张 × num）+ 输出 token 210/M | `1.5 + 229×210/M = 1.548 元` |

**动手做计费前，必须先向上游（阿里云/bella）确认 `gpt-image-2-edits` 实际按哪种扣费。** 响应体两种都支持，不能猜。

## 代理当前处理方式

由 `OBSERVE_MODELS`（逗号分隔，`*` 表示全部）控制，见 `internal/config/config.go` / `.env.example`：

- **放行**：observe 列表内的模型即使**没配价格也不返回 402**，正常转发。
- **观察日志**：每次成功响应打一条结构日志（只打 `usage` + 顶层字段名 + `data` 张数，**不 dump 图像 base64**）：
  ```
  observe gpt-image-2: top_keys=[background created data usage] data_count=1 usage={...}
  ```
- **落库（费用 0）**：`recordUsage` 对 observe 模型用 `cost_cny="0"` 记入 `daily_stats`（`input_tokens`/`output_tokens`/调用次数）。统计页可见调用量，费用 ¥0。
- **未持久化的字段**：`text_tokens` / `image_tokens` / `num` / `size` / `quality` **只在日志里**，没进 `daily_stats`（表是按 key+日期+模型聚合的固定列，`input_tokens` 存的是文本+图片合计）。

## 计费待办（要做时）

1. 先确认 `gpt-image-2-edits` 的真实扣费口径（按张 vs 按 token）。
2. `model_prices` 增补可选费率列：`text_cny` / `input_image_cny` / `output_image_cny`（元/M）、`per_image_cny`（元/张）。
3. usage 解析增补：`input_tokens_details.{text_tokens,image_tokens}`、`output_tokens`、`num`。
4. `calcCost` 按模型已配置的维度累加。
5. （可选，已暂缓）新增按次记录的 `usage_observations` 表，持久化完整 usage 供分析。
