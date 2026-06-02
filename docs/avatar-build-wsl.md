# 自作アバター入りファームを WSL でビルドする手順

プリビルドの `merged-binary` はアバター画像が**黒プレースホルダ**なので、`set_avatar` で
顔が変化しない。自分の顔画像を C 配列に焼き込むには **ESP-IDF でソースビルド**が必要。
ここでは WSL(Ubuntu) + ESP-IDF v5.5.2 でビルドし、Windows 側 esptool で書き込む。

- 画像は事前に14枚（顔6・目3・口5, 320x240 PNG）を用意済みとする
  （`scripts/gen_avatars.py` で生成 → Windows の `C:\Users\<you>\.stackchan\avatar\`）。
- `<you>` は Windows ユーザー名（例では各自読み替え）。
- 所要: 初回 30〜60分（DL/ツールチェーン/ビルド）、ディスク数 GB。

## 0. WSL 前提パッケージ（sudo・1回だけ）

```bash
sudo apt-get update
sudo apt-get install -y git wget flex bison gperf \
  python3 python3-pip python3-venv \
  cmake ninja-build ccache libffi-dev libssl-dev dfu-util libusb-1.0-0
```

## 1. ESP-IDF v5.5.2 を入れる（1回だけ）

```bash
mkdir -p ~/esp && cd ~/esp
git clone -b v5.5.2 --depth 1 --recursive https://github.com/espressif/esp-idf.git
cd ~/esp/esp-idf
./install.sh esp32s3
```

以降、**ビルドするシェルごとに**環境を読み込む（PATH に idf.py 等が入る）:

```bash
. ~/esp/esp-idf/export.sh
```

## 2. ファームのソースを取得（プリビルドと同じ版に合わせる）

```bash
cd ~
git clone --recursive https://github.com/kisaragi-mochi/stackchan-mcp.git
cd ~/stackchan-mcp
git checkout firmware-v1.8.0      # 今書き込んでいる版に合わせる
git submodule update --init --recursive   # firmware/components/smooth_ui_toolkit 等を取得
```

> `--recursive` を付け忘れた場合は、リポジトリのルートで
> `git submodule update --init --recursive` を実行（`smooth_ui_toolkit` 等のサブモジュールが空だと
> CMake が `Failed to resolve component 'smooth_ui_toolkit'` で失敗する）。

## 3. アバター画像を取り込み → local C 配列を生成

```bash
# Windows 側で生成した PNG を WSL ホームへコピー
mkdir -p ~/.stackchan/avatar
cp /mnt/c/Users/<you>/.stackchan/avatar/*.png ~/.stackchan/avatar/

# 変換に Pillow が必要
python3 -m pip install --user pillow

# 変換（既定で ~/.stackchan/avatar を読み、main/boards/stackchan/ に書く）
cd ~/stackchan-mcp/firmware
python3 scripts/avatar_convert/convert_avatars.py
# 生成物: main/boards/stackchan/avatar_images.local.cc / .h （git 無視・placeholder の代わりに使われる）
```

## 4. ビルド（stackchan ボード変種）

```bash
. ~/esp/esp-idf/export.sh         # まだなら
cd ~/stackchan-mcp/firmware
python scripts/release.py stackchan
# ボード設定→set-target→build→merge-bin を一括。
# 生成物: build/merged-binary.bin （と releases/v*_stackchan.zip）
```

> うまくいかない時の手動フォールバック:
> `idf.py set-target esp32s3` → `idf.py build` → `idf.py merge-bin`
> （ボード選択は release.py が行うので、基本は release.py 推奨）

## 5. ビルド成果物を Windows 側へ出す

```bash
cp build/merged-binary.bin /mnt/c/Users/<you>/stackchan-cli/firmware/merged-binary-custom.bin
```

## 6. Windows から書き込み（ボタン不要）

PowerShell:

```powershell
& "$env:USERPROFILE\.local\bin\esptool.exe" --chip esp32s3 --port COM3 `
  --before default-reset --after hard-reset `
  write-flash 0x0 C:\Users\<you>\stackchan-cli\firmware\merged-binary-custom.bin
```

- COM 番号はデバイスマネージャで確認（新ファーム動作中は CDC で COM が出る）。
- 書込み後に画面が黒いままなら **電源 OFF(6s長押し)→ON** でクリーン起動。

## 7. 確認

```powershell
cd C:\Users\<you>\stackchan-cli
$env:VISION_HOST="<PC-LAN-IP>"
.\stackchan-cli.exe repl
# stackchan> avatar happy      ← 今度は画面の顔が変わるはず
# stackchan> avatar surprised
```

## メモ
- 顔を差し替えたいだけなら 3→4→5→6 を再実行（ESP-IDF/前提は再導入不要）。
- `merged-binary` は NVS をリセットするので、書込み後に WiFi/Gateway URL の再設定が必要に
  なる場合がある（設定 AP `Xiaozhi-xxxx` → `http://192.168.4.1` → Advanced タブ）。
  ※ アプリのみ更新でよければ `build/` 内の app バイナリだけを該当オフセットに書く方法もあるが、
    確実なのは merged-binary。
```
