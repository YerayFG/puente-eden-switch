@echo off
rem Compila PuenteEdenSwitch.exe (necesita Go: https://go.dev/dl/)
setlocal
cd /d "%~dp0"
echo Generando icono y manifiesto...
go run ./tools/mkrsrc -o rsrc_windows_amd64.syso assets/icon-256.png assets/icon-64.png assets/icon-48.png assets/icon-32.png assets/icon-16.png || goto :error
echo Compilando...
set GOOS=windows
set GOARCH=amd64
go build -trimpath -ldflags "-s -w -H windowsgui" -o PuenteEdenSwitch.exe . || goto :error
echo.
echo Listo: PuenteEdenSwitch.exe
pause
exit /b 0
:error
echo.
echo Ha fallado la compilacion.
pause
exit /b 1
