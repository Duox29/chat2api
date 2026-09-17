# chat2api — User Guide

Gateway chuyển DeepSeek web chat thành API tương thích OpenAI.
Viết bằng Go (stdlib, không dependency ngoài), login qua Chromium headless có sẵn.

## 1. Yêu cầu

- Go ≥ 1.22
- Node.js + `playwright-core` (chỉ cho `scripts/login.js`; gateway chạy bằng Go, không cần `node_modules/` khi đã có `state/session.json`)
- Chromium của Playwright: `~/.cache/ms-playwright/chromium-*/chrome-linux64/chrome`
- Tài khoản DeepSeek ghi trong `account.json`:
  ```json
  {"account": "email@example.com", "password": "****"}
  ```

## 2. Khởi động (one-click)

- **Linux/macOS:** chạy `./setup.sh`
- **Windows:** double-click `setup.bat` (tự cài Node.js/Go qua winget nếu thiếu)

Script tự làm: `npm install` → tải Chromium → tạo `account.json`
(sửa email/password rồi chạy lại) → `go build` → login lấy token.

Chạy thủ công (nếu không dùng setup):

```bash
cd /home/duox/IdeaProjects/chat2api

# Lần đầu: login qua browser để lấy token (lưu vào state/session.json)
node scripts/login.js
# → {"ok":true,"token_len":64,"cookies":[...]}

# Chạy gateway (mặc định port 8080)
PORT=8081 go run .
# Hoặc build binary:
go build -o chat2api . && PORT=8081 ./chat2api
```

Biến môi trường:

| Biến | Mặc định | Ý nghĩa |
|---|---|---|
| `PORT` | `8080` | Port lắng nghe |
| `GATEWAY_API_KEY` | (trống = mở) | Nếu đặt, client phải gửi `Authorization: Bearer <key>` |
| `REPO_DIR` | `.` | Thư mục repo (chứa `account.json`, `scripts/`, `state/`) |
| `CHROME_PATH` | đường dẫn ms-playwright | Dùng khi Chromium nằm chỗ khác |
| `DSML_DEBUG` | (trống = tắt) | Đặt `1` để log raw DeepSeek response + DSML parse/tool calls ra stderr (truncated, không bao giờ log key/token/cookie). Chỉ bật khi debug. |
| `DSML_ENABLED` | `1` (bật) | Đặt `0` để tắt DSML parser: response DeepSeek trả về nguyên văn text, không bao giờ emit `tool_calls`. |

Gateway tự re-login khi token hết hạn (401/403), không cần can thiệp.

## 3. Models

| `model` | DeepSeek model_type | Ghi chú |
|---|---|---|
| `deepseek-chat` | `default` | Chat thường |
| `deepseek-reasoner` | `default` + thinking | Trả thêm `reasoning_content` |
| `deepseek-expert` | `expert` | |
| `deepseek-vision` | `vision` | |

Tham số mở rộng ngoài chuẩn OpenAI: `thinking_enabled` (bool),
`search_enabled` (bool), `session_id` (string, để chat tiếp nhiều turn).

## 4. Endpoints & ví dụ

Giả sử gateway ở `http://localhost:8081`.

### Health
```bash
curl localhost:8081/health
# {"status":"ok"}
```

### List models — `GET /v1/models`
```bash
curl localhost:8081/v1/models
```

### New session — `POST /v1/sessions`
```bash
curl -X POST localhost:8081/v1/sessions
# {"id":"...","object":"session","seq_id":...,"model_type":"default","created":...}
```

### List sessions — `GET /v1/sessions`
```bash
curl localhost:8081/v1/sessions
```

### Delete session — `DELETE /v1/sessions/{id}`
```bash
curl -X DELETE localhost:8081/v1/sessions/<id>
```

### Chat completion — `POST /v1/chat/completions`
```bash
# Non-stream (mặc định tạo session mới, trả về session_id để dùng tiếp)
curl -X POST localhost:8081/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat",
       "messages":[{"role":"user","content":"Xin chào"}]}'

# Chat tiếp trong cùng session (multi-turn, gateway tự nối parent message)
curl -X POST localhost:8081/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat", "session_id":"<id ở trên>",
       "messages":[{"role":"user","content":"Nói tiếp đi"}]}'

# Stream (SSE chuẩn OpenAI, kết thúc bằng data: [DONE])
curl -N -X POST localhost:8081/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat", "stream":true,
       "messages":[{"role":"user","content":"Kể 1 câu ngắn"}]}'

# Reasoner (có reasoning_content)
curl -X POST localhost:8081/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-reasoner",
       "messages":[{"role":"user","content":"Vì sao bầu trời xanh?"}]}'
```

### Legacy text completion — `POST /v1/completions`
```bash
curl -X POST localhost:8081/v1/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat", "prompt":"2+2 bằng mấy?"}'
```

## 5. Dùng với OpenAI SDK

Chỉ cần đổi `base_url` thành `http://localhost:8081/v1`:

```python
from openai import OpenAI
c = OpenAI(base_url="http://localhost:8081/v1", api_key="x")
r = c.chat.completions.create(model="deepseek-chat",
    messages=[{"role": "user", "content": "Xin chào"}])
print(r.choices[0].message.content, "| session:", r.session_id)
```

## 6. Sự cố thường gặp

| Hiện tượng | Cách xử lý |
|---|---|
| `502 ... browser login failed` | Chạy lại `node scripts/login.js` xem lỗi (có thể dính captcha/rate-limit, chờ vài phút thử lại) |
| Treo lâu ở request đầu | Lần đầu phải giải PoW + có thể re-login, chờ ~10–30s |
| `connection reset` lẻ tẻ | Gateway đã tự retry 3 lần; mạng tới CloudFront đôi khi chập chờn, gửi lại request |
| Muốn nhiều turn hội thoại | Truyền lại `session_id` từ response trước |
| Web DeepSeek cập nhật, API hỏng | Xem `MAINTENANCE.md` (checklist 5 bước) |

## 7. Lưu ý

- `usage` trả `-1` vì web chat không công bố token count.
- `account.json`, `state/` (token/cookie) không commit git (đã có trong `.gitignore`).
