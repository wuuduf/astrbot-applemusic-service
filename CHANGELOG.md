# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- Telegram bot support for Apple Music URL types: song, album, playlist, artist, station, music-video.
- New bot commands: `/playlistid`, `/stationid`, `/mvid`, `/artistid`, `/url`, expanded `/id`.
- New Telegram command: `/chatid` (also `/sessionid`) to show current `chat_id` for whitelist setup.
- Artist secondary flow for `Albums` vs `Music Videos`.
- Unified transfer mode for album/playlist/station: one-by-one and ZIP.
- MV download/send flow with cache-aware re-send.
- Telegram settings support for `alac`, `flac`, `aac`, `atmos`, `aac-type`, `mv-audio-type`.
- `config.example.yaml` and open-source governance files.
- Telegram settings panel now includes an `Exit and delete` button to close the panel quickly.
- Telegram interactive panels now include a unified `Cancel and delete` button (search, paging, transfer mode, artist sub-selection, settings).
- `/artistphoto` (`/ap`) now supports exporting artist profile image plus all album covers and animated covers with one-by-one/ZIP selection.

### Changed
- Telegram cache now supports song audio + MV + ZIP bundle `file_id`.
- Help text and README docs updated to match real bot behavior.
- CI artifacts and Docker config now use safe default config template.
- Bot polling errors now redact Telegram token in console logs.
- Telegram song download transfer now follows per-chat setting (`songzip`) instead of prompting transfer selection.
- Telegram task worker count is now configurable in settings (`worker1` ... `worker4`, default `worker1`).
- AstrBot API download now allows `transfer_mode=zip` for `song` targets.
- Unauthorized reply now includes `chat_id` hint to help configure `telegram-allowed-chat-ids`.
- Telegram auto-cleanup now also considers `AMDL_TMPDIR`/`TMPDIR` when set to dedicated paths (shared `/tmp` and `/var/tmp` are skipped).
- Telegram help text is now Chinese.
- Telegram `/help` now prioritizes short commands (`/h`, `/i`, `/sg`...) while keeping legacy commands compatible.
- `chat_id` is no longer auto-shown in `/start`/`/help` or unauthorized replies.
- `/id` (without args) now shows current `chat_id`; `/id <...>` behavior remains for media downloads.
- Telegram auto extras (`lyrics` / `cover` / `animated`) are now disabled by default for new chat settings.
- Apple API/downloader outbound requests now use a shared HTTP client with configurable timeout (`AMDL_HTTP_TIMEOUT_SEC`, default `45s`).
- `runv2` now uses configurable stream idle timeout (`AMDL_RUNV2_IDLE_TIMEOUT_SEC`, default `300s`) instead of unlimited waits.
- Telegram `song` downloads now always embed lyrics + cover into the audio file; auto extras (`lyrics/cover/animated`) now only affect separate attachment sending.

### Fixed
- `go vet` protobuf lock-copy warning in `utils/runv3/cdm/cdm.go`.
- MV segmented downloader now retries transient segment failures and reports incomplete segment errors clearly instead of producing partial outputs.
- Avoided connection/body handle buildup in paginated Apple API loops by closing response bodies per iteration.
- Improved request failure handling for token/lyrics/webPlayback/download stream paths (clear status checks and decode errors).
