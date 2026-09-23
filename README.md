![Interface](assets/interface.png)

# Usage

Fill out the .env.example and remove the .example

Dockerfile and compose are included 

If you are hosting this behind cloudflare, keep in mind they have a 100MB cap for proxied files.

## Settings

| Key | Default | Meaning |
| --- | --- | --- |
| `USERNAME`, `PASSWORD` | required | Login and HTTP Basic Auth credentials |
| `PORT` | `8080` | Listen port |
| `MAX_FILE_SIZE_MB` | `100` | Maximum size of one file |
| `MAX_FILES_PER_REQUEST` | `20` | Maximum files in one upload request |
| `DEFAULT_TTL_HOURS` | `3` | Expiry used when an upload doesn't set one |
| `MAX_TTL_HOURS` | unset (no maximum) | Longer requested expiries are capped to this |
| `ALLOW_PERMANENT` | `true` | Whether uploads may be marked permanent |

## Upload with curl

Use HTTP Basic Auth with the configured credentials and the `file` field:

```sh
curl -u username:password -F 'file=@./path/to/file' https://your-host/upload
```

The JSON response includes the downloadable URL. Multiple files are supported with repeated `-F file=@...` arguments.

Optional form fields: `ttl_seconds` (expiry in seconds, capped at the server maximum) and `permanent=true`.

The response is `200` when at least one file was stored. Files that failed are listed in `errors` as `{filename, error}`. When every file fails the status is `400` (validation) or `500` (storage), and `413` when the request body is too large.

## Capabilities

`GET /api/capabilities` is public and returns the server's upload limits, so clients can validate before uploading:

```sh
curl https://your-host/api/capabilities
```

```json
{
  "apiVersion": 1,
  "maxFileSizeBytes": 104857600,
  "maxFilesPerRequest": 20,
  "defaultTtlSeconds": 10800,
  "maxTtlSeconds": null,
  "permanentAllowed": true,
  "uploadFields": ["file", "files"]
}
```

`maxTtlSeconds` is `null` when there is no maximum. `apiVersion` increases only when the upload request or response format changes. Responses may be cached for 60 seconds.
