# Testing a live booking

A passing local test or a healthy container does not prove that Yodel issued a
parking pass. A successful live test needs a completed job and the corresponding
Yodel confirmation or wallet pass matching the intended date, pass and vehicle.

1. On Home, configure an OTP source and a Yodel profile. The profile owns its
   login URL and mobile number. Pair BlueBubbles with that profile if used.
2. Create a booking request for that profile, select the date and pass preference
   order, then run **Auth check** and **Dry run**. A dry run checks login, the
   vehicle and pass pages; it stops before adding a pass to the cart.
3. For passes already released, choose **Book now · manual approval**. This
   always requires your approval immediately before final confirmation, even
   if the request uses automatic confirmation for release jobs. Review the
   displayed date, pass and vehicle before approving.
4. Check the completed job and Yodel's issued pass. If the outcome is uncertain,
   inspect Yodel's wallet or confirmation before retrying. The application keeps
   the profile/date reservation after confirmation starts to prevent duplicates.

Book now has a fixed 15-minute limit from enqueue, including queue time, OTP
retrieval and approval. Restarting the application does not extend it or retry
an interrupted action. Availability polling also respects the request's shorter
poll limit. Expired or cancelled attempts that never began confirmation may be
explicitly retried. The application does not clear a pre-existing Yodel cart;
inspect and clear it yourself before another attempt.

**Queue for release** retains the saved preparation, authentication and release
window, including its saved manual or automatic confirmation mode. Testing Book
now verifies the immediate checkout path. A separate release-time test is needed
to verify session warming and the release polling window. Keep unattended
scheduling disabled while testing. `SCHEDULES_ENABLED=false` only prevents
automatic creation of jobs. Jobs created with **Queue for release** still run
with their saved confirmation mode, including automatic final confirmation.
Use **Cancel job** on the job page to stop a queued booking.

BC Hydro says cancelled passes may become available throughout the day, so a
checkout test does not require waiting for the 7 a.m. release. For 2026,
reservations are required from May 14 through September 7; the daily release is
at 7 a.m. for the following day. Check the current
[BC Hydro reservation rules](https://www.bchydro.com/community/recreation_areas/buntzen_lake.html)
and [2026 announcement](https://www.bchydro.com/news/press_centre/news_releases/2026/buntzen-lake-summer-parking-reservations.html)
before choosing a date. Actual inventory can only be determined from Yodel.
Cancel an unused test pass through its reservation confirmation.
