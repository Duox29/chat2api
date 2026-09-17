# chat2api — Maintenance Guide

Tài liệu này ghi lại quá trình build gateway và cách cập nhật khi
DeepSeek web chat (chat.deepseek.com) thay đổi.

## 1. Kiến trúc tổng thể

```
Client (OpenAI API) → main.go → ds/client.go → HTTPS trực tiếp tới chat.deepseek.com
                                        ├── scripts/login.js → Chromium headless (chỉ khi cần token/cookie mới)
                                        └── pow/pow.go → giải PoW mỗi request (~0.3s)
```

Nguyên tắc: **browser chỉ dùng để login** (lấy token + WAF cookies + device
fingerprint). Mọi traffic API còn lại đi bằng HTTPS thuần từ Go, vì đã verify
là WAF chấp nhận khi có đủ cookie.

## 2. Các bước reverse đã làm (làm lại y hệt khi web đổi)

### B1. Lấy danh sách endpoint từ JS bundle

Web DeepSeek là SPA, mọi API đều lộ trong file `main.<hash>.js`:

```bash
# Lấy URL bundle từ HTML trang sign_in, tải về rồi grep
grep -o -E "/api/v0/[a-zA-Z_/]+" main.js | sort -u
```

Quan trọng nhất: `/api/v0/users/login`, `/api/v0/chat_session/create`,
`/api/v0/chat/create_pow_challenge`, `/api/v0/chat/completion`.

### B2. Bắt luồng login bằng Playwright

Dùng Chromium có sẵn (`~/.cache/ms-playwright/chromium-*/chrome-linux64/chrome`)
với `playwright-core`. Script tham khảo: `/tmp/opencode/login2.js`
(không lưu trong repo).

Phát hiện quan trọng: phải **chờ cookie `.thumbcache_*`** (fingerprint
portal101, ~5s sau load trang) rồi mới click Log in, nếu không server trả
`422 body.device_id`.

Kết quả: `POST .../users/login {email, password, device_id(fingerprint),
os:"web"}` → `{user.token}` + header bắt buộc `x-device-id` (UUID),
`x-client-version: 2.5.0`.

### B3. Bắt luồng chat

Script tham khảo: `/tmp/opencode/capture-completion.js`.
Editor là `textarea[placeholder="Message DeepSeek"]` (không phải contenteditable).

Thứ tự gọi:

1. `chat_session/create` → `chat_session.id`
2. `create_pow_challenge {target_path}` → challenge
3. `chat/completion` kèm header
   `x-ds-pow-response: base64({algorithm, challenge, salt, answer, signature, target_path})`

Body completion:

```json
{"chat_session_id": "...", "parent_message_id": null, "model_type": "default",
 "prompt": "...", "ref_file_ids": [], "thinking_enabled": false,
 "search_enabled": false, "action": null, "preempt": false}
```

Stream SSE: event `ready` (lấy `response_message_id`), `data` chứa fragment
(`RESPONSE`/`THINK`) + lệnh `APPEND`, event `title`, event `close` = hết.

### B4. Giải mã PoW worker

Trong `main.js` tìm `n.u(37627)` / `n.u(76608)` + bảng map chunk-id → hash file,
ra URL worker, ví dụ:

- `https://fe-static.deepseek.com/chat/static/76608.8f2a9fa413.js` (bản JS, dễ đọc)
- dep: `https://fe-static.deepseek.com/chat/static/8138.63461459c3.js`

Logic trong `onmessage`: `prefix = salt_expireAt_`, tìm `i` nhỏ nhất sao cho
`H(prefix+i) == challenge`.

Đọc class sponge `U({capacity:256})`: rate 136B, output 32B, padding
`0x06…0x80` (kiểu SHA3), nhưng vòng lặp `for(i=1;i<24;i++)` =
**23 round, bỏ RC[0]**.

Spec cuối (đã validate khớp challenge thực tế với answer=103091):

- **DeepSeekHashV1 = Keccak-f[1600], rate 1088 bit, output 256 bit,
  padding SHA3 (0x06…0x80), 23 round với RC[1]..RC[23], layout lane chuẩn.**

Quy trình validate khi port: viết PoW bằng Python trước, đối chiếu vector thực,
rồi mới port sang `pow/pow.go` + giữ lại test trong `pow/pow_test.go`.

## 3. Khi web DeepSeek đổi → cập nhật chỗ nào

| Dấu hiệu | Nguyên nhân likely | Sửa ở |
|---|---|---|
| Login 422 / không có token | `device_id` fingerprint đổi, field login đổi | `scripts/login.js` + grep `users/login` trong bundle mới |
| `create_pow_challenge` trả shape khác | challenge fields đổi | `ds/client.go` struct challenge |
| PoW solve xong nhưng completion 403 (`INVALID_POW_RESPONSE`) | **thuật toán PoW đổi** → tải worker mới, đọc lại `onmessage`, port lại `pow/pow.go`, chạy `go test ./pow/` | `pow/` |
| Stream parse sai / thiếu chữ | format SSE/fragment đổi (`THINK`→type mới, field `p`/`o` đổi) | `ds/completion.go` hàm `flush` |
| Header `x-client-version` cũ bị từ chối | version bump | hằng `ClientVersion` trong `ds/client.go` (lấy bản mới từ request thật trong DevTools) |
| WAF challenge (202) hàng loạt | TLS/cookie policy đổi | quay lại B2: chạy browser, so sánh header/cookie mới |

## 4. Checklist nhanh khi nghi web đổi

1. `grep -o -E "/api/v0/[a-zA-Z_/]+"` bundle mới → diff endpoint.
2. Tải lại 2 file worker PoW → diff phần `onmessage` (prefix? round? `squeeze`?).
3. Chạy `go test ./pow/` — pass nghĩa là PoW còn đúng.
4. Chạy `node scripts/login.js` — có token nghĩa là login còn đúng.
5. Test 1 completion non-stream → stream → multi-turn.

## 5. Bản đồ file repo

- `main.go` — routes OpenAI-compatible, không chứa logic DeepSeek.
- `ds/client.go` — auth/session/headers/retry/re-login; `ds/completion.go` —
  SSE parser + parent tracking.
- `pow/pow.go` (+ test vector thực) — DeepSeekHashV1.
- `scripts/login.js` — browser login duy nhất.
- Không commit: `account.json`, `state/`, binary (đã có trong `.gitignore`).

## 6. Chạy gateway

```bash
node scripts/login.js        # lần đầu (lưu state/session.json)
PORT=8081 go run .           # hoặc: go build -o chat2api . && ./chat2api
# Env: PORT, GATEWAY_API_KEY (optional), REPO_DIR, CHROME_PATH
```
