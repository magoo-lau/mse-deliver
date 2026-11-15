@echo off
rem deliver-console 双击构建入口（纯 ASCII：cmd 在 chcp 65001 下按字节偏移读含多字节 UTF-8 的行会拦腰截断）
powershell -ExecutionPolicy Bypass -File "%~dp0build.ps1"
pause
