# Plex Artwork Sync for Silo

A Silo plugin that keeps Silo's artwork matching Plex:

- **Posters** for movies and series.
- **Plex collections**, with their posters, members and order.
- Optionally (each off until you turn it on): movie and series **backdrops**, **clear logos**, **season posters** and **collection backdrops**. See [Artwork types](#artwork-types).

It pairs well with [Kometa](https://kometa.wiki): by default it copies the posters Kometa has overlaid, and Kometa's `run_end` webhook starts a sync after every Kometa run. Kometa is optional, though; turn off one setting and give the task a schedule, and it mirrors whatever posters you have in Plex.

It never touches episode artwork, so title cards from AURA or any other tool are left alone. Season posters are only copied if you turn that on. Nothing is ever written to Plex.

*Formerly Kometa Poster Sync.*

## Requirements

The plugin copies from Plex, so it does nothing on its own. You need:

- **A working Plex Media Server** holding the movie and TV libraries you want mirrored, and a Plex token for it.
- **Silo** on Linux (amd64 or arm64), with admin access so you can install plugins and create an admin API key.
- **The same titles in both Plex and Silo.** Items are matched by TMDB id, so a movie or show that's in Plex but not in Silo is skipped.
- **Network access from Silo to Plex.** If you use the Kometa webhook, Kometa must also be able to reach Silo.
- **Kometa** (optional) running against that Plex server, if you want its overlays and an automatic sync after each Kometa run.

## How it works

1. A sync starts when Kometa's `run_end` webhook arrives (after a configurable delay, default 5 minutes), when Silo's scheduled task runs, or when you click Run now. Several webhooks in a row collapse into one run.
2. For each selected Plex movie/TV library, it lists the items to sync: by default those carrying Kometa's `Overlay` label, or every item if you turn that setting off. Each Plex poster path ends in an upload timestamp, so only posters that changed since the last sync are sent.
3. Items are matched to Silo by TMDB id. Each poster is uploaded to Silo as an image file, the same as uploading one by hand in Silo's artwork picker, which locks the item's artwork so a Silo metadata refresh won't replace it.
4. Plex collections are mirrored as manual Silo collections. The plugin only edits or deletes collections it created.
5. If Kometa removes an item's overlay, the clean Plex poster is pushed to Silo once.

The plugin downloads each poster from Plex itself and uploads the image, so Silo never sees a Plex URL and your Plex token stays out of Silo's database.

## Artwork types

Each type is a switch in the plugin settings. Posters are on, as in earlier versions; the rest are off until you turn them on.

| Setting | Plex source | Silo target | Default |
| --- | --- | --- | --- |
| Sync movie and TV posters | the item's `thumb` | item poster | on |
| Sync movie and TV backdrops | the item's `art` (selected background) | item backdrop | off |
| Sync movie and TV logos | the selected clear logo | item logo | off |
| Sync season posters | each season's `thumb`, season 0 (Specials) included | the Silo season with the same number | off |
| Sync collection backdrops | the collection's `art` | collection backdrop | off (needs "Sync Plex collections") |

The plugin copies the image Plex currently shows, whether you picked it, uploaded it, or Kometa set it. It never looks for new art on TMDB or any other provider.

Rules shared by every type:

- **Only what Plex has.** If Plex has no image of a type for an item, Silo's is left as it is; nothing is deleted.
- **Only what changed.** Each type of each item is tracked on its own, so a new backdrop sends only the backdrop. See [Change detection](#change-detection).
- **Same winner as posters.** When a title is in more than one library, every type comes from the copy that wins under [Titles in more than one library](#titles-in-more-than-one-library). If the winner changes, its artwork is sent on the next sync.
- **One upload replaces one type.** Silo's upload replaces only the image of that type; an item's other artwork stays.

### Logos

The plugin reads the logo Plex marks as **selected**. When the library listing includes the item's selected images (an `Image` entry of type `clearLogo`), that is used. Otherwise the plugin asks `/library/metadata/{id}/clearLogos` and uses the one entry marked `selected`; that is one extra Plex request per item per sync, so on such servers logo sync is slower on large libraries. If no entry, or more than one, is marked, or the selected one is a link to another site rather than an image Plex serves, the item's logo is skipped and counted under "Logos with no clear selection". It never takes the first logo on the list.

Logos keep their transparency. A logo is uploaded as Plex serves it (PNG, WebP or JPEG). One over Silo's 10 MiB limit is re-encoded as PNG and, if still too large, scaled down by halves, keeping its alpha channel. A WebP logo over the limit can't be shrunk that way and is skipped with an error. A logo is never turned into a JPEG.

### Season posters

For each series the plugin already matches, it lists the show's seasons in Plex (never its episodes) and matches them to Silo's seasons of that same series by **season number** only. Renamed seasons still match; titles are never compared. A season Plex has but Silo doesn't is skipped and counted under "Seasons not in Silo"; the plugin never creates a season. Silo is asked for a series' seasons only when one of its season posters changed.

Silo serves its season list only to a profile, so the plugin reads it as the primary profile of the account that owns the admin API key (an admin API key needs no profile PIN). It reads nothing else as that profile.

### Collection backdrops

Uploaded with the collection's other updates, from the same copy that supplies the poster (in Combined mode, the winning library). A changed backdrop sends only the backdrop: the collection isn't recreated, its members aren't touched, and its poster isn't sent again. Plex's auto-generated composite images aren't copied.

### Change detection

Plex image paths end in a timestamp (`/library/metadata/123/art/1712345678`) that changes whenever the selected image changes. The plugin records that path, with the Silo item it went to, after Silo accepts the upload. A path without such a timestamp isn't trusted: the image is downloaded and its SHA-256 compared with the one recorded, so an image replaced behind the same URL is still sent.

Limits: a path that keeps its timestamp while its image changes would not be noticed (Plex does not do this for images it stores), and an unversioned path is downloaded on every run to be hashed.

## Install

1. In Silo, create an **unscoped admin API key** (it starts with `sa_`).
2. Download `plex-artwork-sync-linux-amd64` (or `-arm64` on ARM) from the latest [release](../../releases/latest), then in Silo go to Admin → Plugins and upload it.
3. Open the plugin's settings and fill in:
   - **Plex URL**: an address the Silo server can reach Plex at, e.g. `http://plex:32400` if both run in Docker on the same network, or `http://192.168.1.10:32400`.
   - **Plex token**.
   - **Silo admin API key**.
   - **Webhook secret**: any long random string.
   - **Only posters Kometa has overlaid**: leave on if you use Kometa; turn off to sync every poster.
   - The artwork switches (see [Artwork types](#artwork-types)).
4. Open the plugin's **Plex Artwork Sync** status page. Tick the Plex libraries to sync under **Libraries** and click Save (nothing ticked pauses syncing; libraries added to Plex later start unticked). Then click **Run check**. It tests Plex, the Overlay label (when that setting is on), and the Silo API key. (Silo's own "Check connection" button only works for metadata and marker plugins.)
5. Choose what starts a sync: the Kometa webhook (next section), a schedule on the task (see [The scheduled task](#the-scheduled-task)), or both.
6. Turn on **Dry run** for the first run if you want to see what it would change. The summary appears on the status page.

Upgrading from an earlier version: upload the new binary over the old one. Your settings, secrets, library choices and collections carry over. The new artwork types stay off until you turn them on, and posters already synced are not sent again: their records are kept as they were, and the new types are tracked separately.

To try a new type safely, turn it on together with **Dry run**, click Run now, and read the counts on the status page (they show what would be sent), then turn Dry run off.

The artwork types were written against Silo's `main` branch of November 15, 2026 (`images/upload` with `backdrop` and `logo`, season posters, `PUT /admin/collections/{id}/backdrop`, and the season list). Posters and collections need nothing newer than before.

**Requires a Silo build from November 11, 2026 or later** (Silo's `/api/v2`). Version 0.4.0 and earlier use Silo's retired `/api/v1` and stop working on current Silo. If your Kometa webhook URL still contains `/api/v1/plugins/`, replace it with the URL the status page shows (`/api/v2/plugin-content/plugins/…`).

### The Kometa webhook

Add the webhook to Kometa's `config.yml`, keeping any webhooks you already have:

```yaml
webhooks:
  run_end: http://SILO_HOST:PORT/api/v2/plugin-content/plugins/ID/kometa?secret=YOUR_WEBHOOK_SECRET
```

Copy the exact URL from the status page, which fills in this installation's id. Replace `SILO_HOST:PORT` with the address Kometa uses to reach Silo: the container name (e.g. `silo:8080`) if Kometa and Silo share a Docker network, otherwise the server's IP or hostname.

If you already send `run_end` somewhere else, make it a list:

```yaml
webhooks:
  run_end:
    - notifiarr
    - http://SILO_HOST:PORT/api/v2/plugin-content/plugins/ID/kometa?secret=YOUR_WEBHOOK_SECRET
```

**What the Overlay setting covers.** "Only posters Kometa has overlaid" filters **posters only**, by Kometa's `Overlay` label, exactly as before. Kometa labels the items it overlays, which says nothing about backdrops, logos or seasons, so those types are taken from every item in the selected libraries whatever this setting says. After a webhook, only the artwork that changed is sent, for the types you turned on.

Watch for stray quote marks around the URL. A trailing `"` becomes part of the secret, and the plugin rejects the webhook. To test it, POST to the URL yourself (`curl -X POST -d '{}' "URL"`); the status page then shows a sync scheduled. Anything else that can send a POST (a cron job, another tool) can use the same URL.

## Titles in more than one library

Silo has one poster per title, so when the same movie or show is in two Plex libraries (Movies and Movies 4k, say), only one copy's poster is used. Choose which on the status page's **Libraries** card:

- **Highest resolution** (default). Movies compare the actual files Plex reports. TV shows go by library name, since Plex reports no resolution for a show: a library with 4K, UHD or 2160 in its name wins.
- **Lowest resolution**. The same rules, reversed.
- **Library priority**. Number your libraries; the lowest number wins.

Changing the choice re-sends the winning poster for every shared title once, on the next sync. A poster change in a library that isn't the winner leaves Silo alone.

## Collections

Each Plex library's collections go into one Silo library, set under **Collections go to** on the Libraries card. **Automatic** matches by name ("Movies 4k" goes to a Silo library called "Movies 4K"; "TV Shows" to the only non-4K TV library, "TV"). With no match, a collection goes wherever its titles are. Changing the choice moves existing collections on the next sync; nothing is recreated.

### Separate or combined

When the same collection name exists in more than one library ("Marvel" in Movies and Movies 4k), choose on the Libraries card:

- **Separate** (default): one Silo collection per Plex library, each in its own Silo library. The 4K library's collections list only titles you have in 4K, but places that list every collection show both.
- **Combined**: one Silo collection, assigned to each of those Silo libraries, holding every title from both. Its poster comes from the library that wins under "When a title is in more than one library". Opened from the 4K library, it also lists titles you only have in 1080p.

Silo stores a title that's in both libraries as one item with two versions, so the two collections mostly hold the same titles either way. Switching modes moves existing collections on the next sync: combining keeps the winning library's collection and removes the other; separating gives the combined collection back to that library and creates the rest.

Collection order follows Plex. When Plex's order changes (for example Kometa re-ranking a Trending list), the next sync sends Silo the whole new order. In Combined mode the order comes from the copy with the most titles.

The plugin records each collection as soon as Silo creates it, rebuilds one you delete in Silo, and adopts one it created but lost track of (for example after a restart mid-sync) instead of making a duplicate.

**Reset collections** deletes the plugin's collections for the libraries you choose, including any it lost track of, and leaves posters alone. The next sync rebuilds them. Collections you made yourself in Silo are never touched; the plugin only removes collections whose Silo slug starts with `kometa-` (kept from the plugin's original name so existing collections stay tracked).

## While it runs

The status page shows the current step (reading Plex, applying posters, syncing collections) with a running count, and updates by itself. Times show in your own time zone.

**Cancel** on the progress card stops a sync, revert or collection reset within a few seconds. Work finished before that is kept, and the next sync picks up the rest. A sync the webhook has scheduled but not started yet can be called off with **Skip** next to the "Sync at" badge.

If Silo re-keys a title during a sync (it does this when it matches a show better, e.g. `series-imdb-…` becomes `series-tvdb-…`), the plugin looks the title up again and retries. A title Silo no longer has counts under "Not in Silo" rather than as an error.

## The scheduled task

The plugin registers a **Plex Artwork Sync** task in Silo's scheduled tasks. That's the "Run now" button. Silo gives plugin tasks a run-at-startup trigger by default, which is cheap because only changed posters are sent.

Without Kometa, the schedule is what keeps Silo current: set the task to run daily or every few hours. With the Kometa webhook, a daily schedule is an optional safety net.

Silo stops a plugin that has been idle for 30 minutes. While a sync is running or waiting to start, the plugin checks in with Silo every few minutes so it isn't stopped partway through; once there's nothing to do, it lets Silo stop it as usual. Silo starts it again on the next webhook, scheduled run or status-page visit.

## Reverting

Silo has no bulk "reset artwork" action, and the image lock it sets when artwork is applied makes library refreshes skip those items. The status page's **Revert** section handles it per library:

1. Untick the library under **Libraries** and save first, or the next sync pushes the posters again.
2. Tick the libraries, optionally tick "Also delete the Silo collections this plugin created", tick the confirmation, and submit.

For each item the plugin changed, it removes only the image lock (your other locks stay), runs Silo's own metadata refresh so Silo picks its default poster again, and waits for that job before starting more (three at a time). Progress shows on the status page.

A movie in both Movies and Movies 4k is one Silo item, so reverting either library resets it. If you had hand-picked a poster in Silo before the plugin replaced it, revert gives you Silo's default, not that earlier pick.

**Revert works per item, not per artwork type.** Silo keeps one image lock per item, shared by its poster, backdrop and logo, and its refresh re-picks all three; it has no way to reset just one. So reverting an item resets every type the plugin uploaded to it, together, and the plugin forgets them all. A synced season poster is reverted the same way on the season (Silo's refresh of a season resets its poster). There is no "revert only backdrops"; to stop syncing one type, turn its switch off, which leaves what's already in Silo.

## Build from source

Requires Go 1.26.

```sh
go mod tidy
make build      # dist/plex-artwork-sync-linux-amd64 and -arm64
make test
```

## Limits worth knowing

- Items Plex has no TMDB id for are skipped and counted under "No TMDB id".
- Plex "composite" collection posters (the auto collage) aren't copied. Silo builds its own.
- A collection whose items aren't in Silo yet is skipped until they are.
- Posters and backdrops over 10 MiB (Silo's upload limit) are re-encoded to JPEG first; logos are kept transparent (see [Logos](#logos)).
- Images that aren't JPEG, PNG or WebP (for example an error page instead of an image) are refused, not uploaded.
- Silo accepts only a poster for a season; season backdrops and episode images are not synced.
- Banners, disc art, clear art other than logos, and episode stills are not synced.
- Silo reorders at most 200 titles in a collection at once. A larger collection keeps titles in the order they were first added.
- Silo won't delete a collection that a Home or library page row still shows. The plugin reports that as an error until you remove it from the row.

## Troubleshooting

- **"Logos with no clear selection"**: open the item in Plex, pick a logo under Edit → Logo, and sync again.
- **Season posters show "read Silo seasons: … no primary profile"**: the API key's account has no primary profile. Create the key from an admin account that has one.
- **"Seasons not in Silo"**: Silo hasn't scanned that season yet (or doesn't have it). It's checked again on later syncs.
- **Backdrop or logo errors with HTTP 422**: Silo refused the image. The error names the item; check the image in Plex.
- **`… no longer exists; it is looked up again on the next run`**: Silo re-keyed the item during the sync. Nothing is recorded, so the next sync sends it.

### Example log

A sync with every type on, as Silo's plugin log shows it (abridged):

```
artwork applied artwork_type=backdrop title="Dune (2021)" plex_rating_key=4512 silo_item=movie-tmdb-438631 status=applied
artwork applied artwork_type=logo title="Dune (2021)" plex_rating_key=4512 silo_item=movie-tmdb-438631 status=applied
logo skipped: selection not identifiable artwork_type=logo title="Severance" plex_rating_key=8810 reason="Plex marks no single local logo as selected"
season not in Silo artwork_type=season_poster title="Severance" silo_item=series-tmdb-95396 season_number=3 status=skipped
artwork applied artwork_type=season_poster title="Severance" plex_rating_key=8810 silo_item=<season id> season_number=0 status=applied
artwork applied artwork_type=collection_backdrop title="Dune Collection" plex_rating_key=9001 silo_item=c_7f3a status=applied
sync finished trigger=webhook posters_applied=12 backdrops_applied=8 logos_applied=7 season_posters_applied=21 collection_backdrops_applied=1 artwork_unchanged=3412 seasons_not_in_silo=2 errors=0
```

(Values are illustrative; the field names are the ones the plugin logs.)

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE). The bundled Plus Jakarta Sans font is under the SIL Open Font License (`assets/OFL-PlusJakartaSans.txt`).
