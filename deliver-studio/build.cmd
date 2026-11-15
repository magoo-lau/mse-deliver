@echo off
rem deliver-studio full build (Windows double-click entry): runs build.ps1, bypasses execution policy
rem Output: deliver-studio.exe / deliver-studio-linux (this directory)
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0build.ps1"
if errorlevel 1 (echo. & echo BUILD FAILED & exit /b 1)
echo.
pause
