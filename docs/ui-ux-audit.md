# UI and UX audit — September 12, 2026

The audit covers navigation, page hierarchy, forms, action availability, feedback, and responsive behavior across the application. The implementation replaces the oversized settings cards and inconsistent form layout with shared spacing, controls, and page structures.

## Navigation and ownership

| Area | Order and responsibility |
| --- | --- |
| Workspace | Home → Bookings → Jobs |
| Configuration | Lakes → OTP sources → Settings |
| Settings subnavigation | General → Account → Users (administrator only) |
| Home | Lake connection status, upcoming visits, and recent jobs; accounts without a configured lake start on Lakes |
| Lakes | Per-lake provider connection, personal vehicle keyword, release schedule, pass preferences, and advanced booking URLs |
| OTP sources | BlueBubbles/Twilio configuration, connection checks, pairing, and default source selection |
| General settings | Shared preparation, sign-in deadline, retry, and browser defaults |

Account management is no longer an isolated half-width card above the global settings form. Yodel setup lives within Buntzen Lake; Home presents provider-neutral connection status. Saving an account does not mark its connection verified. OTP and browser settings do not appear in lake preferences. Existing requests and queued jobs retain their saved configuration.

## Findings and changes

| Finding | Implemented correction |
| --- | --- |
| Navigation mixed daily work, account administration, and configuration | Two named navigation groups, with Account and Users inside Settings; current-page indication and compact mobile menu |
| Large headings, inconsistent control heights, and orphan cards | Shared typography, 44px text/select controls, consistent labels, spacing, reading widths, and full-width resource rows |
| Settings fields appeared as one unstructured grid | Preparation, Availability and retries, and Browser sections; related values aligned and retry minimum/maximum paired |
| Home setup instructions dominated normal use | Upcoming enabled visits ordered by date, recent jobs, and visual lake connection status; first-time setup starts on Lakes |
| Booking forms exposed implementation settings before essential choices | Visit → account/vehicle → pass preferences → automation; release rules, URLs, and per-request timing overrides under Advanced settings |
| Repeated or competing form actions | One save/cancel footer for resource forms; destructive account and reset actions separated from saving |
| Booking cards offered actions known to be unavailable | Availability depends on release time, visit date, enabled state, pending jobs, and reservation conflicts, with an explanation and relevant next step |
| Pending jobs caused edit forms to offer saves that the server would reject | Busy booking, sign-in, and source forms explain the lock, link to the blocking job, and disable submission; store guards remain authoritative |
| Secondary BlueBubbles source could be impossible to pair when several sign-ins existed | Explicit owned sign-in selection for pairing without changing the user's default source |
| Source status overstated readiness | Separate Default badge and accurate Configured, Paired, or Needs pairing labels |
| Job actions used inconsistent technical names | Sign-in check and Booking rehearsal labels across lists and detail pages |
| Progress could look live after a connection failure | Explicit connecting, connected, reconnecting, and completed states; accessible approval/pairing announcements |
| Error feedback was detached from the form | Inline errors receive focus, preserve non-secret submitted values, and use actionable validation wording |
| Notifications covered controls and tables overflowed mobile screens | Inline notices and responsive job/user rows; controls stack at narrow widths |
| Short desktop windows could hide account actions | Scrollable sidebar, with keyboard-accessible navigation and account controls |
| Lake reset promised values that ignored retained legacy vehicle preferences | Reset copy describes removing overrides and preserving existing requests |

## Lake connection follow-up

The final flow is Lakes → selected lake → provider connection. Provider setup is no longer shown on Home. Buntzen Lake owns its Yodel setup and account actions; OTP source selection and browser/timing defaults retain their global pages. Profiles created by the earlier shared-sign-in UI remain attached to Buntzen without changing their IDs or queued snapshots.

Home redirects to Lakes when the account has no usable configured lake account and default OTP source. After configuration, Home stays available and shows the latest connection evidence. A successful retained job may show “Connection verified” with its check time; this does not promise the provider session is still valid. Profile edits invalidate earlier verification evidence, and pending/failed sign-in states show a corresponding next step.

The follow-up passed the full web suite (110.126s), all 32 client tests, and Go vet. Focused connection, dashboard, lake, and profile checks passed again after the final action-link changes. Rendered checks covered Home, Lakes, the connection panel, and account editing at desktop and 320px/390px widths, including the editor's return link, mobile navigation, and no horizontal overflow. Empty-account handler tests confirmed the Home redirect; isolated renderer fixtures verified the initial lake-selection and OTP setup guidance. The final narrow catalog stacks details so the timezone remains readable.

## Verification scope

Rendered review uses an isolated local preview with sample data. It includes Home, booking list and edit, Lakes and lake preferences, both OTP provider forms and source editing, Yodel sign-in forms, Settings, Account, Users/new-user forms, job history, and terminal job details. Desktop, tablet, and narrow mobile checks inspect layout, navigation, overflow, and control dimensions.

First-run setup, login, member administration, and approval/pairing layouts are also reviewed through synthetic renderer fixtures with disabled actions. These fixtures do not establish successful authentication, pairing, account mutation, or booking checkout.

Automated HTTP and client tests cover ownership, CSRF, pending-resource locks, submission preservation, booking-action gating, default-source selection, multi-identity pairing, stream reconnection/terminal behavior, focus management, and date-aware Home ordering. Browser screenshots and DOM checks complement these tests; they are not a complete assistive-technology or cross-browser certification.

Final results:

- `go test ./internal/web -count=1` passed; the last-added approval HTML assertion also passed separately on the final source.
- `node --test internal/web/testdata/client.test.cjs` passed all 32 tests.
- `go vet ./internal/web` and `git diff --check` passed.
- Pending-resource edit regressions passed a focused race run.
- Rendered checks covered 320px, 390px, 768px, 1280px, and 1440px widths, plus a 450px-high desktop window. The final 320px booking form and Settings validation state had no horizontal overflow; the preview console reported no errors.

No production deployment or history rewrite is part of this UI change. Preview demonstrations do not issue lake reservations or confirm live-provider compatibility.
