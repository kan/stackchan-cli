# stackchan-cli

M5 ｽﾀｯｸﾁｬﾝ（M5Stack 公式 AI デスクトップロボット）を操作する CLI を作るプロジェクト。
本ファイルは **調査ログ / 意思決定記録** であり、方向性確定後に「実装ガイド」へ更新する。

> 公開用に `<device-mac>` / `<PC-LAN-IP>` / `%USERPROFILE%` 等のプレースホルダを使用。
> この環境固有の実値は **git 管理外の `ENVIRONMENT.local.md`** に記録（公開しない）。

## ゴール

同一 LAN 内から ｽﾀｯｸﾁｬﾝを操作する CLI（首振り・表情・LED・発話・カメラ取得など）。
最初に参照した MCP 実装: https://github.com/kisaragi-mochi/stackchan-mcp

## 重要な前提：2つの「ﾌｧｰﾑｳｪｱ世界」がある

所有機は **M5 公式の市販品**（スイッチサイエンス等で購入、ESP32-S3 / CoreS3 系）。
公式アプリの **AVATAR モード**で操作・発話・カメラ取得ができている状態。

### 世界1: 公式ファーム（現状）= クラウド中継方式
公式リポジトリ https://github.com/m5stack/StackChan （`app/` Flutter, `firmware/` ESP-IDF,
`server/` Go バックエンド, `remote/` ESP-NOW リモコン）。

- **デバイスもアプリも WebSocket "クライアント"**。共通サーバ `ws://<host>/stackChan/ws` に
  各自が外向き接続し、**サーバがアプリ↔デバイスを中継（リレー）**。
  → AVATAR/監視カメラ/発話は全部この経路。LAN 直叩き API ではない。
- 認証: `MAC + 乱数 + timestamp` を RSA 暗号化して `authorization` ヘッダ
  （`app/lib/network/web_socket_util.dart`）。ユーザー登録→`v2/device/bind` が土台。
- アプリのホストは **ビルド時差し替えのプレースホルダ**:
  `app/lib/network/urls.dart` → `static const String url = "00.000.000.000:0000/";`
  （コメント例 `192.168.1.100:8080/` = LAN IP）。`getBaseUrl()=http://${url}stackChan/`、
  WS は `ws://${url}stackChan/ws`。→ **OSS 一式はセルフホスト想定**。
- デバイスのファーム/サーバ住所は OTA 経由（`firmware/main/Kconfig.projbuild` の
  `OTA_URL = https://api.tenclass.net/xiaozhi/ota/`）。**ユーザー設定項目ではない（要・再フラッシュ or DNS）**。
- サーバ中継の中核: `server/internal/web_socket/web_socket.go`(25KB) + `socket_task.go`。

### 世界2: stackchan-mcp（xiaozhi 系・別ファーム）
- xiaozhi-esp32 ベースの独自ファーム + Python ゲートウェイ。
- デバイスが WS クライアント、ゲートウェイが WS サーバ（`0.0.0.0:8765`, Bearer token,
  HTTP `:8766/capture` で JPEG 受信、mDNS `_stackchan-mcp._tcp.local.`）。
- handshake: device→`{"type":"hello","version":1,"features":{"mcp":true},...}` /
  server→`{"type":"hello","session_id":"<非空>",...}`（session_id 必須）。
- 以降 `{"type":"mcp","payload":{jsonrpc-2.0}}` で `tools/list` `tools/call`。
  デバイスが MCP サーバとして `move_head`/`set_avatar`/`set_led`/`self.audio_speaker.set_volume`
  等を公開（`McpServer::AddCommonTools` + board の `InitializeTools`）。
- **これを使うには公式ファームを捨ててこのファームを書き込む必要がある。**

## 制御プロトコル（公式・世界1）

メッセージは **`[1 byte MsgType] + payload`**。完全な不透明バイナリではない:
- controlAvatar/controlMotion の payload は **JSON**
  （firmware `app_avatar.cpp`: `updateAvatarFromJson(data)` / `updateMotionFromJson(data)`）。
- jpeg(0x02)=JPEG バイト列、opus(0x01)=Opus 音声。

### MsgType 一覧（`app/lib/model/msg_type.dart`）
| 値 | 名前 | 用途 |
|---|---|---|
| 0x01 | opus | 音声(TTS/通話) |
| 0x02 | jpeg | カメラ1フレーム |
| 0x03 | controlAvatar | 首振り/表情 (JSON) |
| 0x04 | controlMotion | モーション (JSON) |
| 0x05 / 0x06 | onCamera / offCamera | カメラ開始/停止 |
| 0x07 | textMessage | テキスト |
| 0x09–0x0C | requestCall/refuseCall/agreeCall/hangupCall | ビデオ通話 |
| 0x0D / 0x0E | updateDeviceName / getDeviceName | デバイス名 |
| 0x10 / 0x11 | ping / pong | 死活 |
| 0x12 / 0x13 | onPhoneScreen / offPhoneScreen | 画面 |
| 0x14 | dance | ダンス |
| 0x15 | getAvatarPosture | 姿勢取得 |
| 0x16 / 0x17 | deviceOffline / deviceOnline | 在線 |
| 0x18 / 0x19 | onAudio / offAudio | 音声ON/OFF |
| 0x1A | aimedTakePhoto | 狙って撮影 |

※ controlAvatar / controlMotion の JSON スキーマ本体は未確認（要・実機 or `updateAvatarFromJson`
  実装の確認）。stackchan-mcp 側 `move_head` は yaw -90〜90 / pitch 5〜85（firmware で 0〜88 clamp）。

## CLI 化の3方針

| 方針 | 概要 | 長所 | 短所 / 未確定 |
|---|---|---|---|
| **A** 公式クラウドに app なりすまし | M5 本番 `stackChan/ws` に WS 接続、RSA認証+MsgType再現 | 市販ファームのまま | アカウント/RSA認証・本番依存・規約リスク |
| **B** 公式サーバ自前ホスト | OSS `server/` を LAN で建て、デバイスを向け替え、CLI は client 役 | LAN完結・OSSで堅い・MsgType明快 | サーバ構築重い／**デバイス向け替え手段（再フラッシュ/DNS）未確定** |
| **C** stackchan-mcp 独自ファーム | 別ファーム書込み+自前WSサーバ+MCP | プロトコル明快・CLI最易 | 市販AI/AVATAR機能を失う |

現状の方向性: **未決（調査継続中）**。

### 追加判明（重要）: この機種の AI 実体は xiaozhi(虾哥)
- 公式アプリに「MCP」メニューがあり、コピー可能な「Access point address」=
  `wss://api.xiaozhi.me/...`（プロトコル wss、ホスト api.xiaozhi.me）を表示。
- firmware の `OTA_URL = https://api.tenclass.net/xiaozhi/ota/`（tenclass=xiaozhi 運営）とも整合。
  → **公式ファームは xiaozhi-esp32 系。デバイスは標準の xiaozhi MCP プロトコルを喋る。**
- xiaozhi の MCP には紛らわしい2系統:
  - **MCP接入点 `wss://api.xiaozhi.me/mcp/?token=`**: AI プラットフォームが
    `Platform → mcphub → 外部MCPサーバ` と外向きに繋ぐ。接続側は**ツール提供のサーバ役**。
    → **body 制御を外から叩く向きではない**（エージェントに機能を足す用）。
  - **デバイス端 `wss://api.xiaozhi.me/mcp/device/{id}`**: デバイスが自分のツールを公開、
    xiaozhi が呼ぶ。
- ⇒ 新方針 **B'（有力）**: 自前の **xiaozhi-esp32-server** を LAN に建て、デバイスの OTA/接続先を
  そこへ向け替え、**サーバ側からデバイスの MCP ツール(move_head 等)を直接呼ぶ**。
  既知の手順あり（"M5 Stack-Chan Going Local AI: xiaozhi-esp32-server" 等）。
  ただし OTA URL 変更に再フラッシュ or config portal が要る点は世界2(C)と地続き。
- 【確定】実機アプリの Access point は `wss://api.xiaozhi.me/mcp/?token=...`
  = **接入点（ツール提供専用）**。→ これ単体では body を外から叩けない（AI 拡張用）。

## 結論：方針の絞り込み（接入点確定後）
無改造（stock ファーム維持）で CLI から body を直接制御する綺麗な経路は**実質なし**:
- 接入点(`?token=`) … AI にツールを足す向き。body 制御は不可。
- 公式 `server/` の MsgType リレー(AVATARモード) … 使うには Plan A（M5クラウドに app
  なりすまし、RSA認証+アカウント）。脆く規約リスク。

→ 現実的に CLI を成立させる本命は2つ:
- **C（CLI 最易）**: stackchan-mcp / xiaozhi-esp32 系ファームに載せ替え + ローカル
  WSサーバ/ゲートウェイ。LAN完結・プロトコル明快。stock クラウドAIは失う(復元可)。
- **B'（stock相当のAIも残す, 重い）**: 自前 xiaozhi-esp32-server を LAN に建て、
  デバイスを向け替え、サーバ側から device-side MCP ツールを叩く。要 server+LLM。

いずれも「デバイスの接続先を自前に向ける」に **再フラッシュ or config portal or DNS** が要る。
次の分岐: 「再フラッシュ許容か」「ローカル AI インフラを建てる気があるか」。

## 規約・許諾・先行事例の調査（Plan A の可否を中心に）

### 規約での「禁止のされ方」
- **M5 の Terms of Service** は EC ストア向けの汎用規約（shop.m5stack.com/policies/terms-of-service）。
  - リバースエンジニアリング禁止条項 **なし**、非公式クライアント禁止条項 **なし**。
  - 唯一かするのが SECTION 12(k):「to interfere with or circumvent the **security features**
    of the Service」。Plan A は RSA 認証を自前再現するので“回避”と解釈される余地はゼロではない
    （自分の正規アカウント資格でログインする範囲なら回避と言い切れないが、グレー）。
- **xiaozhi.me は利用規約がそもそも公開されていない**（第三者コンプラ調査 OffSeq でも
  「terms of service が無い/アクセス不可」と指摘、score 56/100）。明文の禁止も許可も無いグレー。
- ⇒「この条文で明確に禁止」というものは見つからない。だが明確な許可も無い状態。
  （※法的助言ではない。自己責任の整理）

### 逆に「許可されている／開かれている処理」
- **全部オープンソース**: M5 が firmware/app/server を公開、xiaozhi-esp32(78/xiaozhi-esp32) と
  xiaozhi-esp32-server(xinnan-tech) も OSS。→ 改造・セルフホストは事実上のお墨付き＝王道。
- **MCP接入点**（公式機能）: ツール提供のみ。body 制御は不可。
- ⇒ B'/C はライセンス・規約的にクリーン。Plan A だけがグレー。

### 先行事例（誰が試したか）
- **ローカルAI化（reflash→自前 xiaozhi-esp32-server）**: A-Uta「M5Stack版StackChan：ローカルAI化」
  note 記事ほか複数。手順は **`ota.cc` の `GetCheckVersionUrl()` を自前サーバ(例
  `http://192.168.11.3:8003/xiaozhi/ota/`)に書き換え→ESP-IDF で再ビルド→再フラッシュ**。
  **設定画面/DNS では不可・再フラッシュ必須**。音声会話/カメラ/画像解析は確認、ただし
  **move_head 等の body 制御まで検証した記録は見当たらない**。
- **stackchan-mcp**（kisaragi-mochi）: ローカル MCP ゲートウェイ＋専用ファーム。CLI制御に最も近い既存実装。
- **Plan A（公式クラウドに app なりすまし接続して MsgType を送る）**: **やった形跡が見当たらない**。
  皆 OSS のセルフホストに流れるため、誰もクラウドを叩きに行っていない（＝前例・知見ゼロでリスク高）。

## 未確定・次に潰すべき点
1. デバイスがアバター中継サーバの住所をどう得るか（OTA レスポンス? ハードコード?）。
   → B の「向け替え」を DNS リダイレクトで行えるかが決まる。
2. controlAvatar / controlMotion の JSON ペイロード実スキーマ。
3. 公式本番のバックエンドホスト名（A の接続先、B の DNS リダイレクト対象）。
4. 実機での確認（接続先ホスト、設定画面のサーバ項目有無、ファーム版数）。

## 方針決定（確定）
- **Plan C で進める**: stackchan-mcp のプリビルド firmware を書込み、付属 gateway で疎通確認 →
  CLI は **gateway を叩くクライアント**として整理する。

## 実行環境（重要・確定）
- **gateway も Go CLI も Windows ホストでネイティブ実行する。**
  - 理由: gateway はデバイスが LAN から繋ぐ **WS サーバ(:8765)**。WSL2 は NAT 配下のため
    LAN のデバイスから WSL 内リスナーへ届かない（`netsh interface portproxy` + firewall が必要）。
    Windows ネイティブ実行ならこの面倒が無い。
  - Go CLI は gateway を**子プロセス起動**するので、CLI も同じ Windows ホストで動かす。
- 開発もこの WSL ではなく **Windows ホストの Claude Code セッション**で行う方針。
  - 本 CLAUDE.md(WSL: /home/kan/stackchan-cli) を Windows 側 project へ持ち越すこと。
    （WSL から Windows は `/mnt/c/...`、Windows から WSL は `\\wsl$\...` で相互参照可）

## CLI 実装方針（確定: Go）
- 言語 **Go**（単一バイナリ配布しやすい）。gateway を stdio で叩く **MCP クライアント**。
- 実装スケッチ:
  - gateway を子プロセス起動（`stackchan-mcp`、env: `STACKCHAN_TOKEN`, `VISION_HOST`）。
    stdin/stdout のパイプで JSON-RPC 2.0 を送受信（MCP: `initialize`→`tools/list`/`tools/call`）。
  - MCP クライアントは Go 製ライブラリ（例 mark3labs/mcp-go の client）か、stdio+JSON-RPC を自前実装。
  - サブコマンド例: `status` / `move-head --yaw --pitch` / `avatar <face>` / `led ...` /
    `say <text>` / `photo` → それぞれ対応 tool の `tools/call` に変換。
  - 形態は段階的に: ①まず one-shot(毎回 gateway 起動、再接続待ち、実装最小) →
    ②REPL で常駐 → ③`daemon`+ローカルIPC で one-shot 高速化。

## gateway インターフェースと CLI アーキテクチャ
- gateway が外部に公開するのは **stdio MCP(JSON-RPC 2.0) のみ**（HTTP/SSE 無し）。
  内部に WS `:8765`(デバイス接続) / HTTP `:8766`(JPEG受信) を持つ。
- インストール: `uv tool install stackchan-mcp` または `pipx install stackchan-mcp`。
  起動: `stackchan-mcp`（`--no-mdns` `--check` `--version` 等）。
- 必須 env: `STACKCHAN_TOKEN`（ファーム側 token と一致）。写真用 `VISION_HOST`(PCのLAN IP)。
  `.env` はプロセス起動時に一度だけ読む（変更後は再接続）。
- ⇒ **CLI = gateway を子プロセス起動し stdio で MCP `initialize`→`tools/call` を送る MCP クライアント**。
- 設計上の注意（デバイスは WS クライアントで gateway:8765 に繋ぐ）:
  - 毎コマンドで gateway を起動/終了するとデバイス WS が都度切断→再接続でもたつく。
  - 望ましい形: ①対話REPLで gateway を1回起動し常駐 / ②`daemon` が gateway を保持し
    one-shot コマンドはローカルIPC(名前付きパイプ/ソケット)で daemon に転送 /
    ③簡易版は毎回起動(再接続待ち、遅いが実装最小)。
- 疎通確認の2系統:
  - **gateway 単体**(デバイス不要): README の Python スモークテストが ws://localhost:8765 に
    *デバイスのふりをして* hello/initialize/tools/list を投げ、gateway 自体の動作を確認。
  - **実機込み**: gateway 起動→実機が接続→MCP クライアントから `get_status`=`connected:true`、
    `move_head` 等で確認。MCP クライアントは Claude Code 等でよい（このセッションでも可）。

## Plan C 検証ログ（確定・2026-06-02・実機/ファーム書込み前に実施）

### セットアップ（Windows ネイティブ、完了）
- `uv` を winget で導入（`winget install --id astral-sh.uv`）。uv 0.11.17。
- `uv tool install stackchan-mcp --python 3.12` → **stackchan-mcp 0.8.0**（Python>=3.10 必須。
  手元の `py` は 3.9.5 のみのため uv が cpython-3.12 を自動取得）。
- 実行ファイル: `%USERPROFILE%\.local\bin\stackchan-mcp.exe`。uv tool dir = `%APPDATA%\uv\tools`。
- `stackchan-mcp --check`（要 `STACKCHAN_TOKEN`）→ **ready**。ws:8765 / http:8766 とも AVAILABLE。

### stdio MCP 疎通（デバイス無しで全往復成功）
- スモークテスト: `scripts/smoke_stdio.py`（stdlib のみ、`py` で実行可）。
  newline-delimited JSON-RPC を stdin/stdout で送受信:
  `initialize` → `notifications/initialized` → `tools/list` → `tools/call get_status`。
- 結果:
  - `initialize` OK（`protocolVersion: 2024-11-05`, serverInfo.name=stackchan-mcp）。
  - `tools/list` OK = **静的ハードコード一覧（デバイス接続に非依存）**。後述 `docs/gateway-tools.md` に保存。
  - `get_status` OK = `{"connected": false, "device_id": null, "tools_count": 0}`（**gateway ローカル完結**）。
- ⇒ **B(Go CLI) は実機前に `initialize`/`tools/list`/`get_status` まで完全検証可能**（既知の朗報を実証）。
  実機(A)が要るのは `move_head` 等の実 `tools/call`（未接続だと `"No ESP32 device connected"`）。

### 落とし穴（v0.8.0 実測）
- **`--no-mdns` フラグは存在しない**（usage は `-h` / `-V` / `--check` のみ）。README は古い。
- stdout は **UTF-8 前提**。Windows 既定 cp932 でツール説明（日本語）をデコードすると死ぬ
  → 子プロセスの stdout は UTF-8 でデコードすること（Go では bytes 直読みなので問題なし）。
- WS 認証はヘッダ `Authorization: Bearer <STACKCHAN_TOKEN>`。

### ツール仕様の確定差分（調査メモ補正・実 `tools/list` 由来、計23ツール）
（実スキーマ全文は `docs/gateway-tools.json` に保存。`scripts/dump_tools.py` で再生成可）
- **`move_head` に `speed` 引数は無い**。`yaw`(int -90..90) / `pitch`(int 5..85)、**両方必須**。
  （firmware hard clamp 0..88 を使う `set_head_angles` は *device-side* ツールで、実機接続時のみ出現）
- `take_photo`: `question`(string) **必須**（任意ではない）。
- `set_blink`: 引数 `enabled`(bool)。`set_mouth`: 引数 `mouth` enum `closed|half|open|e|u`
  （メモの `state` は誤り）。`set_avatar`: `face` enum `idle|happy|thinking|sad|surprised|embarrassed|off`。
- `say` / `listen` は **本 build ではフレームワークのみ**。TTS/STT は optional extra
  （`stackchan-mcp[tts-voicevox]` / `[stt-faster-whisper]` 等）を入れエンジン登録するまでエラー。
  （v0.8.0 時点の記録。現在の導入方法は「2026-10-17 更新log」の「聞き取り（listen / STT）セットアップ」）
- 追加の診断/制御ツール: `get_device_info` `get_head_angles` `gpio_test` `uart_diag`
  `check_vm_en` `set_servo_torque` `set_auto_torque_release` `get_touch_state`
  `set_mouth_sequence` `set_led` `set_all_leds` `set_leds` `clear_leds` `set_volume` `set_brightness`。
- env: `STACKCHAN_TOKEN`(必須) / `VISION_HOST`(take_photo用, 未設定だと 127.0.0.1 で実機から到達不可) /
  `VISION_URL` `VISION_TOKEN`(任意)。

## Plan B 実装ログ（Go CLI・2026-06-02・stage-1 one-shot 完了）

### 構成
- `go.mod`: module `stackchan-cli`（go1.26.3）。**依存ゼロ**（標準ライブラリのみ）。
- `internal/mcp/client.go`: stdio MCP クライアント。gateway を子プロセス起動し、
  newline-delimited JSON-RPC 2.0 を stdin/stdout で送受信。バックグラウンドの
  read loop が id でレスポンスを突合（20s タイムアウト）。`Initialize` /
  `ListTools` / `CallTool` を提供。**stdout は []byte 直読みなので cp932 問題は無い**。
- `main.go`: stage-1 **one-shot**（毎コマンドで gateway を起動→1ツール実行→終了）。
  サブコマンド: `status` `tools` `move-head --yaw --pitch` `avatar <face>`
  `led --index --r --g --b` `all-leds --r --g --b` `say <text>`
  `photo --question` `call <tool> [--json '{...}']`（任意ツールの生 JSON 呼び出し）。
- gateway exe 解決順: env `STACKCHAN_MCP_EXE` → PATH の `stackchan-mcp` →
  `~/.local/bin/stackchan-mcp.exe`。`STACKCHAN_TOKEN` 未設定時は dev placeholder を注入し警告。

### 検証（実機なし）
- `go build` OK / `go vet` clean。
- `stackchan-cli.exe status` → `{"connected": false, "device_id": null, "tools_count": 0}`。
- `stackchan-cli.exe tools` → 23 ツールを列挙（gateway の静的一覧と一致）。
- `stackchan-cli.exe move-head ...`（未接続）→ gateway が
  `{"error": "No ESP32 device connected..."}` を返す。
- ⇒ **stdio MCP プラミングは実機前に完全動作**。残りは A 後の実 `tools/call`。

### 既知の制限 / TODO（実機後 or stage-2 で対応）
- gateway はデバイス未接続エラーを **`isError:false`** で返すため CLI の exit code は 0。
  実運用では「connected:false なら非0」等のハンドリングを検討（現状は生レスポンス表示）。
- stage-2: REPL 常駐 / stage-3: daemon + ローカル IPC で one-shot 高速化（CLAUDE.md 既定方針）。
- `say`/`listen` は gateway 側に TTS/STT extra を入れるまでエラー（B の問題ではない）。

## ファーム書き換え手順（Windows）

### 前提
- 対象ボード: **CoreS3 (ESP32-S3)**。USB-C ケーブルで PC と接続（**ベース(底面)側のポート推奨**）。
- COM ポートが出ない場合は CP210x / CH9102 等の USB シリアルドライバを入れる（M5 のダウンロードページ）。
- **CoreS3 ダウンロードモード**: リセットボタンを約2秒長押し→内部の緑LED点灯で離す→書込待機。

### 純正に戻せる安心材料
- **M5Burner** で StackChan を検索→Download→Burn すれば**いつでも出荷時(公式AI)ファームに復元可能**。
- （任意）焼く前にフルバックアップ: `esptool --chip esp32s3 --port COM3 read_flash 0x0 0x1000000 backup.bin`

### Plan C（stackchan-mcp）= 一番楽
1. Releases から最新 `firmware-v*` の **`merged-binary.bin`** を入手（ビルド不要）。
2. Python + esptool 用意: `pip install esptool`。COM ポートはデバイスマネージャで確認。
3. ダウンロードモードにして書込み:
   `esptool --chip esp32s3 --port COM3 -b 460800 write_flash 0x0 merged-binary.bin`
4. 起動後、デバイスの **WiFi 設定 UI で「WebSocket Gateway URL」** を CLI/ゲートウェイを動かす
   PC の `ws://<PC-IP>:8765/` に設定（**再フラッシュ不要・NVS 保存**）。
   - ※ ビルド時に焼くなら Kconfig `CONFIG_DEFAULT_WEBSOCKET_URL`、直書きなら NVS `websocket.url`。
5. PC 側で gateway（または自作 CLI の WS サーバ:8765 / HTTP:8766）を起動 → 接続を待ち受け。
- ソースからビルドする場合（任意）:
  `docker run --rm -v $PWD:/project -w /project espressif/idf:v5.5.2 python ./scripts/release.py stackchan`
  → `releases/v*_stackchan.zip`。

### Plan A 実機書込み実測ログ（2026-06-02・所有機で実施・成功）
- **所有機の USB は ESP32-S3 ネイティブ USB（VID 303A / PID 1001, USB mode=USB-Serial/JTAG）**。
  CH9102 ブリッジは介さない。MAC `<device-mac>`、Flash **16MB**、ESP32-S3 rev v0.2。
  ⇒ CLAUDE.md 旧記述「CH9102/CP210x ドライバ」「ベース側ポート」は所有機には当てはまらない。
- **通常ファーム起動中はネイティブ USB が JTAG モード**で COM が出ない（`USB JTAG/serial debug unit`
  のみ）。**ダウンロードモードに入ると CDC(MI_00) が現れ COM 割当**（実機では COM3）。
- **ダウンロードモード投入**: 側面ボタン約2秒長押し→緑LED点灯で離す（**3秒超で電源OFFになるので注意**、
  実際1回やらかした）。「画面が点く=USB接続あり」ではない（バッテリ駆動）。COM の有無で判定すべき。
- **esptool は `--before no-reset --after no-reset` 必須級**。既定の reset を打つとダウンロードモードを
  抜けてしまい `Could not open / busy / doesn't exist` になる。手動で bootloader に入れた後は no-reset。
- **read_flash は不安定**: 「最初の1回の read は成功、2回目以降の read で "The chip stopped responding"」。
  flash-id 等の短命令は毎回OK。⇒ **フルバックアップ(16MB read)は事実上困難**（1MBチャンク方式でも
  chunk1 で停止）。**write_flash は単発の持続転送で安定**（9.98MB→圧縮2.7MB を46.6sで書込み, hash検証OK）。
  結論: 生バックアップは諦め、復元は **M5Burner**(公式)に委ねる判断で書込み実行。
- **書込みコマンド（実際に成功した形）**:
  `esptool --chip esp32s3 --port COM3 --before no-reset --after hard-reset write-flash 0x0 firmware/merged-binary-v1.8.0.bin`
  （v5.3.0。`read-flash`/`write-flash` はハイフン形。`merged-binary` は 0x0 単発で可）。
- **進捗ログは必ずファイルへ**（`*> flash.log`）。esptool の per-block 進捗が端末出力を溢れさせ、
  フォアグラウンドだと harness に途中 kill される（exit 2 に見える）。background+log 推奨。
- 使用 firmware: **firmware-v1.8.0 の `merged-binary.bin`**（`firmware/merged-binary-v1.8.0.bin`）。
- 書込み後リブートで **COM3(CDC) が常時 OK 表示**に変化＝新ファーム起動の目印（stock は JTAG のみ）。
- 次の作業: 実機側 WiFi 設定 UI で **WebSocket Gateway URL = `ws://<PC-LAN-IP>:8765/`**（このPCのLAN IP）
  を設定 → gateway 起動 → `stackchan-cli status` で `connected:true` を確認 → `move-head` 等。

### Plan B'（自前 xiaozhi-esp32-server, 重い）
- `ota.cc` の `GetCheckVersionUrl()` を自前サーバへ書換え → ESP-IDF で再ビルド → 再フラッシュ。
  設定画面/DNS では不可（A-Uta 記事の手順）。サーバ+LLM 構築も要る。

## Plan C 実機接続・制御 成功ログ（2026-06-02・全工程完走）

### 結論（再現手順の要点）
1. **書込みはボタン不要が判明**: `esptool --chip esp32s3 --port COM3 --before default-reset --after hard-reset write-flash 0x0 merged-binary-v1.8.0.bin`。
   ネイティブ USB-Serial/JTAG は esptool の **default-reset** で自動的に bootloader へ入る
   （CDC の COM が出ていれば、側面ボタンの「2秒→緑LED」格闘は不要だった）。
   ※ ただし COM が出るのは①ダウンロードモード中 or ②アプリが CDC を出す状態。アプリが JTAG のみだと COM 無し。
2. **mDNS 自動検出は本環境では機能せず**:
   - PyPI 版 gateway 0.8.0 は mDNS 広告コードを**持たない**。
   - GitHub main 版（`uv tool install --force "git+https://github.com/kisaragi-mochi/stackchan-mcp#subdirectory=gateway"`）は
     `serve` サブコマンド＋`--no-mdns` を持ち mDNS 広告する。が、PC の多 NIC で **14個の IP（169.254×11, 172.x, 100.x, <PC-LAN-IP>）を広告**し、
     `HOST=<PC-LAN-IP>` を渡すと**広告を .70 単独に絞れた**ものの、それでもデバイスは接続せず
     → **WiFi ルーターが無線クライアント宛マルチキャストを通さない**等で mDNS 不通と判断。
3. **確実な経路＝デバイスに URL 明示**: captive portal `http://192.168.4.1` の **Advanced タブ**で
   **WebSocket Gateway URL = `ws://<PC-LAN-IP>:8765/`**（末尾 `/`）、**Gateway Token = 空**。
   基本タブは SSID/パスワードのみ。**Advanced タブを開かないと URL 欄は出ない**（最初これで詰まった）。
   設定モードへ戻すのは merged-binary 再書込み（NVS リセット）が確実（画面タップでは入れない機種だった）。
4. **gateway 側トークンは空でよい**（`STACKCHAN_TOKEN` 未設定 = "accept any client"。ファームの空 token と一致）。
5. **Windows Firewall**: inbound 許可が必須（デバイス→PC へ着信）。管理者 PowerShell で
   `New-NetFirewallRule ... -Protocol TCP -LocalPort 8765,8766` と `-Protocol UDP -LocalPort 5353`。

### 接続確認（成功）
- `stackchan-cli wait` → `connected: true, device_id: "<device-mac>", initialized: true, tools_count: 21`。
- デバイス側 MCP ツール（実機接続時のみ出現、`self.` 接頭辞）: `self.robot.set_head_angles` /
  `self.robot.get_head_angles` / `self.robot.set_servo_torque` / `self.display.set_avatar` /
  `self.display.set_mouth` / `self.led.set_color` / `self.led.set_all` / `self.camera.take_photo` /
  `self.audio_speaker.set_volume` / `self.screen.set_brightness` / `self.touch.get_touch_state` 等 計21。
- 実制御成功: `stackchan-cli avatar happy` → `{"face":"happy","ok":true}`、
  `stackchan-cli all-leds --r 0 --g 0 --b 255` → `{"available":true,"ok":true}`（顔・LED が実際に変化）。

### one-shot の弱点（要 stage-2/3）
- gateway を毎回起動/終了するため、**コマンド毎にデバイスが再接続するまで待つ**（リトライ間隔が長く 30s では不足、
  **device コマンドは接続を最大90sポーリングしてから tool 実行**するよう CLI 実装済み `waitConnected`）。
- 体感を上げるには **常駐 gateway（stage-2 REPL / stage-3 daemon+IPC）**が本筋。one-shot は「起動→90s待ち→実行」で遅い。
- `wait [--timeout N]` コマンド = gateway を保持して `connected:true` を待つ（疎通確認・デバッグ用）。

### stage-2 REPL（実装済み・2026-06-02）
- `stackchan-cli repl`: gateway を**1回だけ起動して常駐**、初回だけ `connected` を待ち、
  以降は対話プロンプト `stackchan> ` で各コマンドを**即時実行**（再接続待ち無し）。
- 実装: 各 `cmdX(args, c *mcp.Client)` を永続クライアント対応に統一（`c!=nil` で reuse・device 待ちスキップ、
  `c==nil` で従来の one-shot）。共通化に `withClientOrReuse` / `callAndPrint(..., outer *mcp.Client)`。
  シェル風 `tokenize` でクォート対応（`say "..."`, `call <tool> --json '{...}'`）。FlagSet は ContinueOnError（REPL で os.Exit させない）。
- 実機確認: REPL に status→avatar happy→all-leds→move-head→avatar idle を流し込み、
  **デバイス接続を維持したまま全コマンド即応答**（90s 再接続待ちが消えた）。
- 残: stage-3 daemon + ローカル IPC（バックグラウンド常駐で one-shot コマンドも高速化）。

### avatar が画面に出ない（プリビルド firmware の仕様・実機確認）
- 症状: `avatar <face>`（`off` 含む）は `ok:true` を返すが**画面に変化なし**。move-head / LED は正常。
- 原因: 公開プリビルド `merged-binary` の `firmware/main/boards/stackchan/avatar_images.cc` が
  **真っ黒の RGB565 プレースホルダ**（顔画像は権利上同梱不可）。よって set_avatar は効くが描画が空。
  画面のニコニコ顔は **xiaozhi 標準 emoji**（set_avatar 管轄外の別レイヤー）。
- 見える顔にするには **ソースからビルドが必要**:
  1. PNG を `~/.stackchan/avatar/` に置く → 2. `cd firmware && python scripts/avatar_convert/convert_avatars.py`
  （`avatar_images.local.cc/.h` を生成、git 無視。存在すれば placeholder の代わりに使われる）
  3. ESP-IDF/Docker でビルド（`espressif/idf:v5.5.2 ... release.py stackchan`）→ 4. 再フラッシュ（`--before default-reset`）。
- ⇒ CLI 制御は全系統 OK。avatar 画像だけはプリビルドの制約で、ソースビルド案件。
- **【実施・成功 2026-06-02】** WSL + ESP-IDF v5.5.2 で自作アバターを焼き込み、`avatar <face>` で
  **実機画面の表情が切り替わることを確認**。手順: `docs/avatar-build-wsl.md`、画像生成: `scripts/gen_avatars.py`。
  ハマり所: ①`git clone --recursive`（`smooth_ui_toolkit` サブモジュール）②`rm -rf build` 後に
  `python scripts/release.py stackchan` ③Pillow は venv 内なら `--user` 無しで install。
  書込みは `--before default-reset`（ボタン不要）。merged-binary は NVS リセット → WiFi/URL 再設定要。

### 起動・運用メモ
- 実行: `STACKCHAN_TOKEN` 空 + `VISION_HOST=<PC-LAN-IP>` で `stackchan-cli <cmd>`。
- gateway 子プロセスは新版で **`serve`（既定 `--transport stdio`）** 起動（CLI 実装済み）。
- esptool hard-reset 後に画面が黒いことがある（USB DTR がブートモード保持）。**電源 OFF→ON で正常起動**。

## stage-3 daemon + タッチ反応log（2026-06-02・成功）
- `daemon.go`: gateway を常駐保持し ①頭部タッチに自動反応 ②`127.0.0.1:8770` の TCP IPC で
  `stackchan-cli <cmd>` を即時転送（**~0.04s**、gateway 再起動・ポート競合なし）。
- **【旧方式・廃止】** 出力配線: main.go に `var out io.Writer = os.Stdout`。結果 print を `out` 経由に。
  daemon は `withOutput(w, fn)`（`execMu` で直列化）で out を IPC conn / stderr に切替。
  → 「2026-10-17 更新log」の「daemon のコマンドの並行実行（#1）」で `cmdEnv` 方式に置き換え済み。
- 転送: main() の one-shot 経路で `sendToDaemon(os.Args[1:])` を試行→ daemon があれば送信、
  無ければ従来の自前 gateway 起動にフォールバック。`quoteArgs` で空白入り引数を再クォート。
- **【旧方式・廃止】** 以下のポーリング方式は「2026-10-17 更新log」の通知方式に置き換え済み（`--touch-poll`・`touchWatch` は削除）。
- タッチ検知: `get_touch_state` を `--touch-poll`(既定150ms) で監視。返り値
  `{zone0/1/2:bool, raw, last_event, last_event_age_ms}`。**ファームの tap/stroke ラベルは
  stroke に偏る**ので使わず、`raw>0||zone*` の**接触時間で自前分類**：release≤1s=tap /
  それ超=stroke、1.8s ホールドで接触中に stroke 発火。2s デバウンス（`touchWatch`）。
- **実機知見**: ①ヘッドタッチは **zone0/1/2 の特定スポット**に**手のひらで大きく覆う**と発動しやすい
  （指先のなで/位置ズレだと raw=0 のまま無反応＝センサ自体は正常）。②ファーム分類は stroke 偏重 →
  接触時間ベースの自前判定で tap/stroke を出し分け可能に。tap 上限は 1s が体感良。
- 反応は**スクリプト化**: `~/.stackchan/reactions/<gesture>.txt`（初回 daemon 起動で雛形生成）。
  あればその中身を `doSource`/`runLine` で実行（avatar/led/say/sleep 等）、無ければ組み込み既定。
  **再ビルド不要**で発火時に都度読む。`--reactions <dir>` で場所変更。`reactToGesture`/`seedReactions`。
- 運用注意: daemon の再起動時は **gateway 子プロセス(8765/8766) が完全に落ちてから**でないと
  次の gateway が "closed stdout" で失敗する。ポート(8765/8766/8770)空きを確認してから起動。
- daemon 稼働中は **one-shot も `repl` も IPC 転送**（共通 `sendToDaemon`）。`repl` は起動時に
  `daemonAvailable()` を見て、あれば各行を転送（`replLoop(remote=true)`）、無ければ自前 gateway。
  ※ readline は非 TTY（パイプ）だと行を読まない＝対話端末でのみ。one-shot のパイプ/スクリプトは従来どおり。

## TTS（発話）セットアップlog（2026-06-02・成功）
- `say` = gateway が **VOICEVOX で合成 → Opus 化 → WS でデバイスへ送出 → スピーカー再生**（ファーム変更不要）。
- **GitHub main 版 gateway は VOICEVOX エンジン実装内蔵**（`tts/voicevox.py`、起動時自動登録）。
  既定 URL `http://127.0.0.1:50021` / `STACKCHAN_VOICEVOX_URL` で上書き。既定話者 3（ずんだもん）。
- 必要物: ①VOICEVOX エンジン起動(:50021) ②gateway に `opuslib`（当時は `uv tool install --force --with opuslib ...`。
  現在は `[tts]` extra に含まれる。導入コマンドは「2026-10-17 更新log」を参照）
  ③**Windows 用 `opus.dll`**（opuslib が `find_library('opus')` で PATH 探索）。
  → **PyPI の PyOgg 同梱 libopus を流用**: `uv run --with pyogg python -c "...shutil.copy(pyogg/opus.dll, ~/.local/bin/opus.dll)"`。
  検証: `opuslib.Encoder(16000,1,APPLICATION_VOIP).encode(...)` が通れば OK。`~/.local/bin` はユーザー PATH 上。
- **口パクが止まらない不具合 → 修正済み**: one-shot が say 直後に gateway を Kill し、末尾の
  `{"type":"tts","state":"stop"}`(=Speaking 解除) が届かず口パク継続。`mcp.Client.Close()` を
  **グレースフル化**（stdin 閉→正常終了待ち、Kill は 2s フォールバック）で stop が flush され解決。
  副次的に one-shot 終了が ~0.6s と高速化。
- 個人設定（この CLI 既定）: **既定話者 8=春日部つむぎ**（`gatewayEnv` が `STACKCHAN_VOICEVOX_DEFAULT_SPEAKER=8` 注入、
  env or `say --speaker N` で上書き）、**発話後に自動 `set_avatar embarrassed`**（`say --face <name>`/`--face ""` で変更/無効）。
- 手順詳細: `docs/tts-voicevox.md`。話者一覧: `GET :50021/speakers`（ずんだもん3 / つむぎ8 / めたん2 等）。

## 2026-10-17 更新log（firmware 1.17.0 / gateway 0.18.0・IP変更対応・VOICEVOX自動起動）

> 本節が最新。上の「stage-3 daemon + タッチ反応log」のポーリング方式、出力配線（`execMu` での直列化）、運用注意（ポート解放待ち）、v1.8.0 前提の記述は本節で置き換え。

### daemon の常駐方式（repo 外・この PC の設定）
- タスクスケジューラ `StackchanDaemon`（ログオン時）が
  `wscript.exe ~/.stackchan/start-daemon.vbs` を実行 → 画面なしで `stackchan-cli.exe daemon` を起動し、
  出力を `~/.stackchan/daemon.log` に追記（各行に時刻付き）。gateway の出力は `~/.stackchan/gateway.log`
  （起動時に 5MB を超えていれば `.1` に移す。3 秒ごとの「Processing request of type」の行は捨てる）。
- **タスクの「失敗時に再起動」（1 分間隔で 3 回）は効かない**（2026-10-20 に使い捨てタスクで実測: 終了コード 1 で
  終わっても再実行されない。タスク自体を起動できなかったときだけの設定）。代わりに:
  - daemon は gateway が起動しない（`initialize` に答えない）とき、10 秒おきに 3 回まで自分で試し直す。
    実行ファイルが無いなど、試し直しても直らない失敗は再試行しない（終了コード 1）。IPC の窓口は試すたびに
    開いて閉じる（待ちの間、コマンドは単発の gateway に切り替わる）。
  - `gateway.log` への書き込みの失敗は無視する（`dropLines`）。失敗を返すと gateway の stderr を読む処理が止まり、
    パイプが詰まって gateway が止まるため。
  - gateway が途中で終了したら（`mcp.Client.Done()`）、daemon も終了する（壊れたまま動き続けないように）。
  - そのどちらも終了コード 2（`daemonExitRestart`。Go のパニックも 2）。`start-daemon.vbs` は 2 のときだけ 60 秒後に
    起動し直す（15 分に 5 回を超えたらやめる）。`Stop-Process` による停止や「already running」は 2 ではないので、
    起動し直さない（実測済み）。
- gateway は `--no-mdns` で起動する（`STACKCHAN_MDNS=1` で戻せる）。mDNS の登録は毎回の起動に約 1.4 秒かかり
  （約 3.1 秒 → 1.8 秒）、本体は URL を直接指定して繋ぐので使っていない。
- 停止: **`Stop-ScheduledTask` では daemon が止まらない**（タスクが終わっても `stackchan-cli.exe daemon` は
  残る。実測）。one-shot や REPL のプロセスも同じ名前なので、コマンドラインで daemon を見分ける:
  `Get-CimInstance Win32_Process -Filter "Name='stackchan-cli.exe'" | Select ProcessId,CommandLine`。
  末尾が `daemon` のプロセスを `Stop-Process -Id <PID>` で止める。
  起動: `Start-ScheduledTask -TaskName StackchanDaemon`。daemon が残ったまま起動すると、新しい daemon は
  「already running」で終了し、古いバイナリが動き続ける。
  バイナリ差し替えは停止 → `go build -o stackchan-cli.exe .` → 起動 → daemon.log で起動時刻を確認。
- CLI は起動時に自分を Job オブジェクト（KILL_ON_JOB_CLOSE）に入れる。daemon を強制終了しても
  gateway・Python・VOICEVOX は道連れで終了し、ポートは残らない（実測済み）。
  daemon 起動時は 8765/8766 が空くまで最大 15 秒待つ（直前の gateway の解放待ち）。

### PC の IP 変更でデバイスが繋がらなくなった件
- 原因: PC の Wi-Fi IP が DHCP で変わり、デバイスの `websocket.url` が古い IP のまま。
  ルーターで PC の新しい IP を予約済み（実値は `ENVIRONMENT.local.md`）。
- **captive portal(192.168.4.1) は設定モード中しか開けない**。設定モードへは「起動中の表示の間に画面を
  短くタップ」（起動後のタップは音声聞き取り）だが、実機ではうまく入れなかった。
- **USB 経由で NVS の `websocket.url` だけ書き換えて解決**（Wi-Fi 設定は保持）:
  `py scripts/nvs_set_ws_url.py --port COM3 --url ws://<PC-IP>:8765/`
  （読み出し→バックアップ `~/.stackchan/nvs-backup/`→エントリと CRC 書き換え→`0x9000` へ書き戻し）。
  NVS は `0x9000` から 16KB、文字列エントリのヘッダ CRC とデータ CRC は `zlib.crc32(x, 0xFFFFFFFF)`
  で既存 23 エントリ全一致を確認済み。元より長い URL は in-place 不可。
- firmware 1.11 以降はデバイス接続中なら `call gateway_config_set --json '{"url":"..."}'` で変更可（推奨）。
- デバイスの LAN IP は DHCP。ping の待ち時間 0.5 秒の一斉検索では見つからず、2 秒にして ARP に出た。
- USB ケーブル/ポートによっては給電のみで `VID_303A` が出ない。PC 直挿しで COM3 が出た。

### firmware / gateway 更新（v1.8.0 → v1.17.0、gateway → 0.18.0）
- firmware はアプリだけ更新: `0x20000` に `xiaozhi.bin`、`0x800000` に merged-binary から切り出した
  assets（v1.8 と中身が違うため）。パーティション表は同一、otadata は ota_0 を指していた。NVS は保持。
  バイナリは `firmware/v1.17.0/`（git 管理外）。
- gateway: `uv tool install --force --python 3.12 "stackchan-mcp[tts]==0.18.0"`（`[tts]` に opuslib を含む。
  `opus.dll` は従来どおり `~/.local/bin`）。
- 新機能の使い方:
  - **タッチ通知**: `legacy_event.enabled: true` の notify.yml を `STACKCHAN_NOTIFY_CONFIG` で渡すと、
    gateway が `stackchan/event`（`event_type`/`subtype`/`duration_ms`）を stdio に通知する。
    daemon はこれで反応（ポーリング廃止）。firmware の分類は 400ms 以上を stroke とするので、
    daemon は `duration_ms` ≤ 1000ms を tap とし直す。
  - **アバター**: build 時埋め込みは廃止され、`load_avatar_set`（layered = 14 枚 × 160×120 RGB565 LE、
    537,600 バイト）で実行時に送る。デバイスの RAM にのみ保持 → daemon が新 session ごとに再送。
    デバイスは gateway の `VISION_HOST:8766/avatar_set/<id>` を GET する。
  - gateway 経由で呼べるのは対応表にあるツールのみ。`self.wifi.set_power_save` は直接呼べない。

### 未解決: デバイスからの応答が遅い・届かない
- ping は ~10ms だが、デバイス往復の MCP 呼び出し（`get_touch_state` 等）が 0.3〜10 秒、
  ときに gateway の 10 秒タイムアウト（`Timeout waiting for ESP32 response`）。v1.8 でも v1.17 でも発生。
- `load_avatar_set` は GET 完了・顔も表示されるのに、デバイスの `avatar_set_loaded` 返信が届かず
  `device_timeout` になることがある（2 秒で `ok:true` が返る回もある）。発話直後の `set_avatar` も
  タイムアウトしやすい。口パクより声が遅れる。
- 次の手: Wi-Fi 省電力（`WIFI_PS_MIN_MODEM` 既定）の影響確認。

### タッチ反応の取りこぼし調査（2026-10-17）
- 本体が検出したタッチは通知で daemon に届いている（シリアルログと daemon.log の突き合わせで確認）。
- 取りこぼしの主因は「反応の最中のタッチ」。daemon は 1 件だけ覚えて反応後に処理する
  （新しいものが来たら置き換え）。無視・置き換えたタッチは daemon.log に `touch ignored` / `touch replaced` と出る。
  直前に反応したタッチから 1 秒以内のタッチ（1 回の接触の二重報告）と、10 秒以上待たされたタッチは捨てる。
- 発話後の表情戻し（`set_avatar`）の待ち時間を短くしても効果は無い（デバイスは命令を順に処理するので、
  次の `clear-leds` が同じだけ待たされる）。既定の 20 秒のまま。
- 6 分半放置した後も、最初のなでるは即検出された（放置でセンサーが止まる挙動は再現せず）。
- USB を抜き差しした直後の数分間、なでても Si12T の値が 0 のままだった。静電容量センサーの
  基準値がアースの変化でずれ、自動で合わせ直すまで反応しにくい可能性（未検証）。
- 本体との往復は USB 接続中・バッテリーのみとも中央値約 500ms（USB は遅延の原因ではない）。
- 反応で LED とサーボが動くたびに USB シリアル（COM3）が一瞬切れる。消費電力の急増で電源が揺れている可能性。
- デバイスのシリアルログは DTR/RTS を下げたまま開けばリセットされない（pyserial で `dtr=False`/`rts=False` を
  open 前に設定）。内部 SRAM の空きは 43KB 前後、最小 22KB。

### 聞き取り（listen / STT）セットアップ（2026-10-17・成功）
- gateway: `uv tool install --force --python 3.12 "stackchan-mcp[tts,stt-faster-whisper]==0.18.0" --with "av<19"`。
  **`av<19` 必須**: PyAV 19.0.1 は faster-whisper 1.2.1 の `av.open(..., metadata_errors="ignore")` を受け付けず、
  `open() got an unexpected keyword argument 'metadata_errors'` で文字起こしが失敗する（18.1.0 で解消）。
- CLI: `stackchan-cli listen [--sec N] [--lang ja] [--model M] [--motion none|face-only|look-up]`（`listen` ツールを呼び、
  結果の `text` を `heard: ...` と表示）。CLI は gateway に `STACKCHAN_FASTER_WHISPER_MODEL=small` を渡す
  （`base` では「スタックチャン」→「スタッフちゃん」、`small` で正しく取れた。CPU で文字起こし約 4 秒）。
- `--motion face-only`（聞き取り中に thinking 顔）は、デバイスの応答遅れで `set_avatar` がタイムアウトすると
  listen ごと失敗し、直後に WS が切れたことがある。既定は `none`。
- 周囲の音（YouTube 等）も拾う。テスト時は止める。

### VOICEVOX 自動起動
- エンジンが応答しなければ CLI が `vv-engine\run.exe` をウィンドウ無しで起動（約 2〜4 秒で準備完了）。
  daemon は起動時に立ち上げて保持。詳細は `docs/tts-voicevox.md`。
- daemon が起動するエンジンは GPU（`--use_gpu`、DirectML）で動かし、起動直後に一度合成して慣らす（2026-10-20）。
  one-shot の `say` は CPU のまま（毎回エンジンを起動・終了するので、GPU の初回の準備の方が高くつく）。
  「2026年10月20日、火曜日、11時24分です」の合成が CPU 約 1.1 秒 → GPU 約 0.1 秒（初回のみ約 4.7 秒）。
  gateway の `say` は文全体を合成し終えてから実時間のペースで送り、送り終えてから返るので、合成時間がそのまま
  声が出るまでの無音になる。`voice "今何時"` の合計は 7.3 秒 → 6.4 秒（うち読み上げ 5.5 秒）。
  GPU で起動や慣らしの合成ができなければ CPU で起動し直す。`STACKCHAN_VOICEVOX_GPU=0` で CPU。
- daemon.log の時刻はミリ秒まで出す（遅れの原因を切り分けるため）。
- 本体の音量は既定で 100（最大）。gateway に音量倍率の設定は無い。

### daemon のコマンドの並行実行（#1）
- IPC のコマンドとタッチの反応は互いを待たずに並行で動く。出力先はコマンドごとの `cmdEnv`
  （client と `out` / `errOut`）で渡す。グローバルの `out` / `errOut` / `withOutput` / `execMu` は削除済み。
  **出力先をグローバル変数に戻さない**（並行実行で出力が混ざる）。
- 反応の途中に IPC のコマンドが割り込むのは許す（ユーザー決定）。反応どうしは `reactToTouches` の
  1 本のループで順に実行し、重ならない。デバイスは命令を届いた順に処理する。
- 例外: スピーカーとマイクを使う `say` / `listen` のツール呼び出しだけは `audioMu` で 1 つずつ実行する
  （聞き取り中の発話が録音される、2 つの発話の音声が混ざるのを防ぐ）。
- IPC の応答で `errOut` に書いた行は `ipcStderrPrefix` 付きで送り、クライアントは stderr に書く
  （one-shot と同じ出し分け）。
- 実測: `listen --sec 10`（計 13.4 秒）の最中に `status` / `avatar` が 0.03〜0.12 秒で返った。
  `listen` 中の `say` は `listen` の終了を待ってから話した。

### 音声コマンド（#4）
- きっかけは本体の画面タップ（ユーザー決定）。firmware は画面タップで聞き取りを始め、もう一度のタップで
  音声を送る。gateway 0.18.0 はこの「本体側で始まった聞き取り」の音声を、`STACKCHAN_AUDIO_HOOK_URL` が
  設定されていれば Ogg/Opus で POST する（`audio_input_hook.py`）。設定が無ければ捨てる。文字起こしはしない。
- daemon が `127.0.0.1:8771/voice` で受け、gateway の Python で faster-whisper を実行して文字にする。
  Python は `%APPDATA%\uv\tools\stackchan-mcp\Scripts\python.exe` で、`STACKCHAN_STT_PYTHON` で上書きできる。
  設定は gateway の `listen` と同じ（`small`、beam_size=1）。Python は daemon が常駐させる（`stt.go`、下記）。
- 照合のルールは `~/.stackchan/voice-commands.txt`（1 行 1 ルール、上ほど優先、`*` は当たらなかったとき）。
  照合の前に、全角カタカナはひらがな、全角英数は半角、大文字は小文字にそろえ、記号と空白は除く。
  呼びかけの言葉（「スタックチャン」）は必須にしない（ユーザー決定。聞き取りが不安定なため）。
- 文字起こしの間は thinking の顔にし、終わったら idle に戻してからルールを実行する（ユーザー要望）。
  既定の `*` ルールは悲しい顔で謝ったあと、2 秒で idle に戻す。
- `stackchan-cli voice "<文>"` で、話さずにルールを試せる。文なしの `stackchan-cli voice` はルールの一覧を表示する。
- 文字起こしは 60 秒で打ち切る（Python が固まると、以後の音声コマンドが受け付けられなくなるため）。
  文字起こしに失敗したときも `*` ルールを実行し、失敗が本体の反応で分かるようにする。
- ルールの中で `voice` は使えない（`cmdEnv.inVoice`。呼び合いが続くと daemon ごと落ちるため）。
- 小さな音や無音は、Whisper が「何だろう? 何だろう? …」のような繰り返しを返すことがある（文字起こしに 13 秒かかった）。
- `say` の文の `{date}` `{weekday}` `{time}` は話すときの値に置き換わる（`expandSayVars`）。既定のルールの「何時」で使う。
  検索や自由な会話は issue #2 の追加タスクで Claude に任せる（ユーザー決定）。

### 標準の表情・画面のオートオフ
- 標準の表情は `embarrassed`（ユーザー要望。`main.go` の `restFace`）。切り替えるのは、`say` のあと、
  音声コマンドの文字起こしのあと、daemon が本体の接続を見つけたとき（本体の起動時は `idle`。眠っている間は切り替えない）。
- 画面のオートオフは `set_off_timeout`（秒、0 で無効、既定 300）。本体の NVS に保存される。
  この PC の本体は 0（無効）に設定済み（2026-10-17、ユーザー要望）。
- `call <tool> --json '{...}'` の順で書くと、以前は `--json` が読まれず空の引数で呼んでいた（Go の flag は
  最初の位置引数で解析を止める）。`parseCallArgs` はツール名の前と後ろの両方でフラグを読むよう修正済み。

### 文字起こしの常駐と GPU（2026-10-20）
- 音声コマンドの文字起こしは、daemon がモデルを読み込んだ Python を 1 つ常駐させて行う（`stt.go` の `sttWorker`）。
  1 行の JSON `{"path": ...}` を送り、`{"text": ...}` か `{"error": ...}` を受け取る。daemon の起動時に読み込み、
  60 秒答えが無いか落ちたら止めて、次の音声で起動し直す。以前は毎回 Python を起動していた（読み込みに 1.5〜3 秒）。
- GPU: RTX 2070 SUPER（8GB）。CUDA のライブラリは gateway の環境に
  `uv pip install --python %APPDATA%\uv\tools\stackchan-mcp\Scripts\python.exe nvidia-cublas-cu12 "nvidia-cudnn-cu12==9.*"`
  で追加した。**gateway を `uv tool install --force` で入れ直すと消える**ので、そのときは README の `--with` 付きの
  コマンドを使う。CTranslate2 は DLL を PATH からしか探さないため、`whisperEnv`（`stt.go`）が
  `site-packages\nvidia\{cublas,cudnn,cuda_nvrtc}\bin` を PATH に足し、device=cuda / float16 にする（文字起こし係と
  gateway の `listen` の両方）。cuBLAS と cuDNN の両方が無ければ CPU のまま。文字起こし係は GPU に失敗したら CPU に
  切り替えるが、gateway の `listen` には切り替えが無い（gateway は改造しない）。GPU で困ったら
  `STACKCHAN_FASTER_WHISPER_DEVICE=cpu` で両方 CPU に戻せる。
- 文字起こし係の制限時間: モデルの読み込みは 5 分（初回のダウンロードを含む）、1 回の文字起こしは 60 秒。
  止めるときは `taskkill /T` でプロセスの木ごと止める（uv の `python.exe` は本体の Python を子として起動するため）。
- 実測（`small`、2.3 秒の音声）: CPU int8 で約 2.2 秒、GPU float16 で約 0.17 秒。GPU は初回だけ CUDA の準備に
  1.5〜8.6 秒かかるので、文字起こし係は無音で一度文字起こししてから準備完了を返す。
  音声の受信から結果まで: 常駐前 4〜5 秒 → CPU 常駐 約 3 秒 → GPU 約 1 秒。
- 「スタックチャン」は `small` だと「スタッグちゃん」になる（照合では呼びかけを必須にしていないので実害は無い）。

### 天気（issue #6・2026-10-20）
- `say` の `{weather}` / `{weather_tomorrow}`（`weather.go`）。取り先はユーザー決定で、気象庁の予報 JSON を主にし、
  今の気温だけ Open-Meteo で足す。「明日の天気」にも答える（ユーザー決定）。
- 気象庁の JSON（`forecast/<office>.json` の 1 つめのレポート）: `timeSeries[0]` が地方ごとの天気の文（全角空白区切り、
  `speakableWeather` で詰める）、`[1]` が 6 時間ごとの降水確率（その日の残りの最大を読む）、`[2]` が地点ごとの気温
  （00:00 が朝の最低、09:00 が日中の最高）。11 時・17 時の発表では、発表日の 00:00 に最高と同じ値が入る（実測:
  11 時発表で「27」「27」）ので、発表日の最低は 09 時より前の発表のときだけ使う。05 時・17 時の発表の実物はまだ
  保存していない（テストは形を絞った JSON で書いた）。
- 「今日」は発表日ではなく今の日付で決める（キャッシュが日付をまたいでも正しい日を読む）。
- 地域の設定は `~/.stackchan/weather.txt`（リポジトリ外）。**住んでいる地域は repo に書かない**（この CLAUDE.md も公開）。
  この PC の地域は `ENVIRONMENT.local.md`。緯度・経度は住所から国土地理院の住所検索 API で求め、小数第 2 位に丸める。
  テストの保存 JSON も、利用者の地域ではなく東京（`testdata/jma-forecast-130000-1100.json`）にしている。
- 実測: 「今日の天気は」の読み上げが約 10 秒（文が長い）。予報の取得は初回のみ約 2 秒、以後はキャッシュ。
  長いので（ユーザー要望）、部分ごとのプレースホルダー `{weather_sky}` `{weather_temp}` `{weather_rain}` と、
  それぞれの `_tomorrow` を用意し、既定のルールで「天気だけ」「気温」「雨/傘」に割り当てた（各 3〜5 秒）。
- 読みの修正（ユーザー指摘）: VOICEVOX は語をつなげると読みが崩れる（`audio_query` の `kana` で確認できる）。
  地名は文の中で読み違えることがある（設定の `name` をひらがなにする。実例は `ENVIRONMENT.local.md`）、「晴れ所により」→「しょにより」、
  「一時雨」→「いちじう」、「後」→「ご」、「平地」→「ひらち」、かなの語が前の語にくっつく（「晴れところ」→「はれれ」）。
  `speakableWeather` の `jmaReadings` で言い換え、くっつく語の前には「、」を入れる。
- 17 時以降（「今夜の〜」）は最高気温を読まない。降水確率は終わっていない 6 時間枠だけで最大を取る。
  取れなかったプレースホルダーだけを「ごめんね、天気が取れなかった。」にし、同じ文のほかの部分は残す。
- 既定の音声ルール: 「明日の気温」「明日は雨」などの個別ルールのあとに「明日|あした」で明日の天気全体を答える
  （「明日は雨降る？」が今日の降水確率を答えないように）。既存の `voice-commands.txt` は上書きされないので手で足す。
- 2026-10-20 12:07 に daemon の起動が `initialize: timed out after 20s`（gateway が 20 秒応答しない）で 1 回失敗した。
  直後の再起動は約 3 秒で成功。gateway の出力を捨てていたので原因は分からない（「強制終了の直後の起動」を 3 回
  再現しても起きなかった）。対策は「daemon の常駐方式」の節（起動の再試行、gateway.log、--no-mdns、VBS の再起動）。

### 夜間の睡眠（2026-10-20）
- 夜中に約 52 分おきに「なでる」（2〜6 秒）の誤検出が 2 回ずつ続き、反応していた（daemon.log で確認）。
  原因は未調査。
- daemon `--sleep HH:MM-HH:MM`（既定 `00:00-07:00`、`off` で時間帯なし、`sleep.go`）。眠る間は
  `set_touch_sensor_enabled false`（NVS 保存。本体自身のタッチ動作と通知の両方が止まる）と
  `reactions/sleep.txt`（LED 消灯、`brightness 0`）。起きるとセンサーを戻して `wake.txt`
  （`brightness 100`、`avatar embarrassed`）。
- `stackchan-cli sleep`（引数なし）で時刻に関係なく眠り、`wakeup` まで起きない（ユーザー要望）。
  時間帯の中で `wakeup` すると、その時間帯が終わるまで起きている。状態は daemon のメモリのみ。
  `sleep <ms>` は従来どおり一時停止（メタコマンドから `dispatch` の `cmdSleep` へ移動）。
- 再接続時: 眠っていれば sleep.txt を再実行。起きていればセンサー有効化と `set_avatar restFace` のみで、
  wake.txt は daemon 起動後の最初の接続だけ（手動で変えた明るさを Wi-Fi の切断のたびに戻さないため）。
  失敗（本体のタイムアウト）は 30 秒ごとにやり直す。センサーは成功するまで、スクリプトは最大 5 回。
  本体が未接続の間は試さない。gateway は本体のタイムアウトを `{"error": ...}` の通常の結果で返すので、
  スクリプトの実行時だけ `cmdEnv.deviceErrors` でこれを失敗として数える（他のコマンドの終了コードは従来どおり）。
- タッチの反応と sleep の適用は `sleeper.stepMu` で直列にする（眠る瞬間の反応の LED が残らないように）。
- 引数なしの `sleep` はスクリプト内でも眠る（音声ルール「おやすみ => sleep」などに使えるよう、あえて残している）。
- 実機で確認済み: 眠る間 `get_touch_sensor_enabled` = false。`sleep` → `wakeup` でも同じく切り替わる。
- 寝顔（ユーザー要望）: firmware の表情は 6 種類で増やせないので、14 枚すべてを寝顔にした一式
  `~/.stackchan/avatar-sleep/`（`scripts/gen_avatars.py --sleep`）を眠るときに送り、起きるときに通常の一式に
  戻す（各 2〜5 秒）。眠る間の明るさは 10（ユーザー確認済み）。
- アバター一式の送信は `sleeper.loadAvatar` だけが行う（接続のたびと、眠る・起きるの切り替え時。
  `watchSessions` は送らない）。スクリプトに `avatar-load` を書かないこと（二重送信と競合の元になった）。
  送信は再試行せず、`stepMu` の外で行う（タッチの反応を止めないため）。

## 参照ファイル早見
- 公式アプリ通信: `app/lib/network/{urls,http,web_socket_util}.dart`, `app/lib/model/msg_type.dart`
- 公式デバイス: `firmware/main/apps/app_avatar/app_avatar.cpp`, `.../view/ws_call.cpp`,
  `firmware/main/Kconfig.projbuild`, `firmware/main/hal/board/stackchan_camera.*`
- 公式サーバ: `server/internal/web_socket/{web_socket,socket_task}.go`, `server/README.MD`
- 別ファーム: kisaragi-mochi/stackchan-mcp `gateway/`, `firmware/docs/{websocket,mcp-protocol}.md`
