# upstream（stackchan-mcp）の更新を取り込む

gateway と firmware は [kisaragi-mochi/stackchan-mcp](https://github.com/kisaragi-mochi/stackchan-mcp) のリリースを使う。
GitHub の Watch を Releases にして通知を受け、取り込みは月 1 回くらいにまとめる。

## 1. 変更を読む

- `CHANGELOG.md` の該当する版を読み、次の機能への影響を確かめる。
  - `say` / `listen`（引数、既定値、環境変数）
  - タッチの通知（`stackchan/event` の形、tap / stroke の判定）
  - 音声の hook（`STACKCHAN_AUDIO_HOOK_URL`）
  - アバターの送信（`load_avatar_set`）
  - ツール名（`tools/list`）
- 各リリースの「Compatibility」にある、推奨する gateway と firmware の組み合わせに合わせる。

## 2. daemon を止める

```
Get-CimInstance Win32_Process -Filter "Name='stackchan-cli.exe'" | Select ProcessId,CommandLine
Stop-Process -Id <末尾が daemon のプロセスの PID>
```

`Stop-ScheduledTask` では止まらない（CLAUDE.md「daemon の常駐方式」）。

## 3. gateway を入れ直す

```
pwsh scripts/install-gateway.ps1 -Version <版>
```

`uv tool install --force` は環境を作り直すので、スクリプトは `av<19` と CUDA のライブラリもまとめて入れ直し、
最後に版と依存の版を表示する。`opus.dll` が無ければ警告する。既定の版はスクリプトの `$Version` で、上げたら書き換える。

## 4. firmware のアプリ部分を書き込む

自分でビルドするまで（issue #11）は、ビルド済みのリリースのアプリ部分だけを書き込み、NVS（Wi-Fi と gateway の URL）を残す。

```
gh release download firmware-v<版> -R kisaragi-mochi/stackchan-mcp -p merged-binary.bin -p xiaozhi.bin -D firmware/v<版>
esptool --chip esp32s3 --port COM3 --before default-reset --after hard-reset write-flash 0x20000 firmware/v<版>/xiaozhi.bin *> firmware/v<版>/flash.log
```

- `firmware/` は git 管理外。
- assets（`0x800000` から後ろ）が前の版と違うときだけ、`merged-binary.bin` の `0x800000` から後ろを切り出して書き込む。
  1.17.0 と 1.18.0 は同じだった。パーティション表（`0x8000`）が変わっていたら、アプリだけの書き込みはしない。
- **書き込んだあと、もう一度本体を再起動する**（`esptool --chip esp32s3 --port COM3 --before default-reset --after hard-reset chip-id`）。
  1.18.0 の書き込み直後は本体の通信が遅く、アバターの送信に 90〜110 秒かかった（再起動後は 3 秒）。

## 5. daemon を起動して確かめる

```
go build -o stackchan-cli.exe .
Start-ScheduledTask -TaskName StackchanDaemon
```

毎回、次の項目を試す。

| 項目 | 試し方 | 期待する結果 |
|---|---|---|
| 接続 | `stackchan-cli status` | `connected: true`。`get_touch_state` などに新しい版の項目が出る |
| アバター | `stackchan-cli avatar-load` | 数秒で `"ok": true` |
| 発話 | `stackchan-cli say "テスト"` | 声が出て、口が動き、終わると標準の表情に戻る |
| 聞き取り | `stackchan-cli listen --sec 5` で話す | `heard: ...` に話した文が出る |
| 頭のタッチ | 頭をタップ、なでる | daemon.log に `touch tap` / `touch stroke` が出て反応する |
| 音声コマンド | 画面をタップして「今何時」と話し、もう一度タップ | 時刻を答える |
| 眠る／起きる | `stackchan-cli sleep` → `call get_touch_sensor_enabled` → `wakeup` | `enabled` が false → true。寝顔と通常の顔に切り替わる |
| タイマー | `stackchan-cli timer 15s` | 15 秒後に知らせる |

## 6. 記録を直す

- README の「前提」の版と、`scripts/install-gateway.ps1` の既定の版。
- CLAUDE.md の最新の更新ログ（版、変わった挙動、試した結果）。
