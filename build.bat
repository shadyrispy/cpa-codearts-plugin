@echo off
REM Build the CodeArts Doer CLIProxyAPI plugin on Windows.
REM
REM The plugin ABI is a C ABI, so CGO and a C toolchain (MinGW-w64 gcc or MSVC
REM with the C++ workload) are mandatory. Set CC if gcc is not on PATH.
REM
REM Usage: build.bat
setlocal

cd /d "%~dp0"

set OUT_DIR=plugins\windows\amd64
if not exist "%OUT_DIR%" mkdir "%OUT_DIR%"

echo building plugin -^> %OUT_DIR%\codearts-provider.dll

set CGO_ENABLED=1
set GOOS=windows
set GOARCH=amd64

go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o "%OUT_DIR%\codearts-provider.dll" .
if errorlevel 1 (
  echo.
  echo BUILD FAILED. If CGO could not find a compiler, install MinGW-w64 and set:
  echo   set CC=C:\path\to\mingw64\bin\gcc.exe
  exit /b 1
)

echo.
echo done. Verify the ABI exports with:
echo   objdump -p %OUT_DIR%\codearts-provider.dll ^| findstr cliproxy
