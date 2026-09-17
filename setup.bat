@echo off
REM chat2api one-click setup (Windows, double-click to run)
REM Installs: Node.js + Go (via winget if missing), npm deps,
REM Playwright Chromium, Go binary, DeepSeek login.
setlocal EnableDelayedExpansion
cd /d "%~dp0"

echo [setup] 1/6 checking prerequisites...

where node >nul 2>nul
if errorlevel 1 (
  echo [setup] Node.js not found, installing via winget...
  winget install -e --id OpenJS.NodeJS.LTS --accept-source-agreements --accept-package-agreements
  if errorlevel 1 (
    echo [setup] ERROR: cannot install Node.js. Get it at https://nodejs.org
    pause & exit /b 1
  )
  set "PATH=%ProgramFiles%\nodejs;%PATH%"
)
where go >nul 2>nul
if errorlevel 1 (
  echo [setup] Go not found, installing via winget...
  winget install -e --id GoLang.Go --accept-source-agreements --accept-package-agreements
  if errorlevel 1 (
    echo [setup] ERROR: cannot install Go. Get it at https://go.dev/dl
    pause & exit /b 1
  )
  set "PATH=%ProgramFiles%\Go\bin;%PATH%"
)
node --version
go version

echo [setup] 2/6 installing npm dependencies...
call npm install
if errorlevel 1 ( echo [setup] ERROR: npm install failed & pause & exit /b 1 )

echo [setup] 3/6 installing headless Chromium (Playwright, one-time download)...
call npx --yes playwright-core install chromium
if errorlevel 1 ( echo [setup] ERROR: chromium download failed & pause & exit /b 1 )

echo [setup] 4/6 checking account.json...
if not exist account.json (
  copy account.example.json account.json >nul
  echo [setup] created account.json from example - EDIT IT with your DeepSeek email/password, then re-run setup.bat
  pause & exit /b 0
)

echo [setup] 5/6 building Go gateway...
call go build -o chat2api.exe .
if errorlevel 1 ( echo [setup] ERROR: go build failed & pause & exit /b 1 )

echo [setup] 6/6 logging in via headless browser (one-time, may take ~30s)...
call node scripts\login.js
if errorlevel 1 (
  echo [setup] ERROR: login failed (wrong password / captcha / rate-limit). Fix account.json and re-run.
  pause & exit /b 1
)

echo [setup] DONE. Start the gateway with:
echo     chat2api.exe
echo   (optional port:  set PORT=8081 ^& chat2api.exe)
echo   test:  curl localhost:8081/health
pause
