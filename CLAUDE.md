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

### 起動・運用メモ
- 実行: `STACKCHAN_TOKEN` 空 + `VISION_HOST=<PC-LAN-IP>` で `stackchan-cli <cmd>`。
- gateway 子プロセスは新版で **`serve`（既定 `--transport stdio`）** 起動（CLI 実装済み）。
- esptool hard-reset 後に画面が黒いことがある（USB DTR がブートモード保持）。**電源 OFF→ON で正常起動**。

## 参照ファイル早見
- 公式アプリ通信: `app/lib/network/{urls,http,web_socket_util}.dart`, `app/lib/model/msg_type.dart`
- 公式デバイス: `firmware/main/apps/app_avatar/app_avatar.cpp`, `.../view/ws_call.cpp`,
  `firmware/main/Kconfig.projbuild`, `firmware/main/hal/board/stackchan_camera.*`
- 公式サーバ: `server/internal/web_socket/{web_socket,socket_task}.go`, `server/README.MD`
- 別ファーム: kisaragi-mochi/stackchan-mcp `gateway/`, `firmware/docs/{websocket,mcp-protocol}.md`
