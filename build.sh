#!/bin/sh
# Windows 向けビルド: dist/nico-dl.exe (CLI) と dist/nico-dl-gui.exe (C 製 Win32 GUI)
# 必要: Go、x86_64-w64-mingw32-gcc (apt: gcc-mingw-w64-x86-64)
set -eu
cd "$(dirname "$0")"
mkdir -p dist
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/nico-dl.exe ./cmd/nico-dl
x86_64-w64-mingw32-gcc -municode -mwindows -O2 -Wall -Wextra -o dist/nico-dl-gui.exe gui/main.c \
  -lshell32 -lole32 -lcomdlg32 -s -static-libgcc
echo "built: dist/nico-dl.exe dist/nico-dl-gui.exe"
