![Interface](assets/interface.png)

# Usage

Fill out the .env.example and remove the .example

Dockerfile and compose are included 

If you are hosting this behind cloudflare, keep in mind they have a 100MB cap for proxied files.

## Settings

| Key | Default | Meaning |
| --- | --- | --- |
| `USERNAME`, `PASSWORD` | required | Login and HTTP Basic Auth credentials |
| `API_KEY` | unset (disabled) | Key accepted in the `x-api-key` header on every authenticated endpoint |
| `PORT` | `8080` | Listen port |
| `MAX_FILE_SIZE_MB` | `100` | Maximum size of one file |
| `MAX_FILES_PER_REQUEST` | `20` | Maximum files in one upload request |
| `DEFAULT_TTL_HOURS` | `3` | Expiry used when an upload doesn't set one |
| `MAX_TTL_HOURS` | unset (no maximum) | Longer requested expiries are capped to this |
| `ALLOW_PERMANENT` | `true` | Whether uploads may be marked permanent |
| `HASH_LENGTH` | `13` | Characters in generated file IDs (4–64, lowercase base32). Existing files keep their IDs |

## Upload with curl

Use HTTP Basic Auth with the configured credentials and the `file` field:

```sh
curl -u username:password -F 'file=@./path/to/file' https://your-host/upload
```

The JSON response includes the downloadable URL. Multiple files are supported with repeated `-F file=@...` arguments.

Optional form fields: `ttl_seconds` (expiry in seconds, capped at the server maximum) and `permanent=true`.

The response is `200` when at least one file was stored. Files that failed are listed in `errors` as `{filename, error}`. When every file fails the status is `400` (validation) or `500` (storage), and `413` when the request body is too large.

## chibisafe-compatible uploads

`POST /api/upload` accepts [chibisafe](https://github.com/chibisafe/chibisafe) upload requests, so ShareX configs, browser extensions and scripts made for chibisafe work when pointed at teacup. Set `API_KEY` and send it as `x-api-key`:

```sh
curl -H 'x-api-key: your-api-key' -F 'file[]=@./shot.png' https://your-host/api/upload
```

```json
{"name": "c2e4jfzcc2iny.png", "uuid": "c2e4jfzcc2iny", "url": "https://your-host/c2e4jfzcc2iny.png", "thumb": ""}
```

Each request uploads exactly one file. Errors use chibisafe's shape: `{"statusCode": 413, "error": "Request Entity Too Large", "message": "..."}`. Uploads use the default expiry, and the optional `ttl_seconds` and `permanent` fields also work here. The `albumuuid` header is ignored. Chunked uploads (`chibi-*` headers) are rejected with `400`.

A ShareX custom uploader (`.sxcu`) for teacup:

```json
{
  "Version": "14.0.0",
  "Name": "teacup",
  "DestinationType": "ImageUploader, FileUploader",
  "RequestMethod": "POST",
  "RequestURL": "https://your-host/api/upload",
  "Headers": { "x-api-key": "your-api-key" },
  "Body": "MultipartFormData",
  "FileFormName": "file[]",
  "URL": "{json:url}"
}
```

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
