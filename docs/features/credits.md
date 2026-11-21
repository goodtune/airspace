# Credits and query scope

How Airspace spends OpenSky credits, and how it stops you running out.

## Credit arithmetic

A free registered OpenSky account has 4,000 credits a day. The cost of one
`/states/all` request depends on the area asked for:

| Area | Cost |
|---|---|
| 25 sq° or less | 1 credit |
| 25 to 100 sq° | 2 credits |
| 100 to 400 sq° | 3 credits |
| more than 400 sq°, or the whole world | 4 credits |

Credits used in an hour:

| Interval | 1 credit | 2 credits | 3 credits | 4 credits |
|---|---|---|---|---|
| 5 s | 720 | 1,440 | 2,160 | 2,880 |
| 10 s | 360 | 720 | 1,080 | 1,440 |
| 60 s | 60 | 120 | 180 | 240 |

The defaults assume two hours of use a day:

- At the default 10 s, two hours costs at most 2,880 credits, even zoomed
  all the way out.
- At 5 s, two hours of a regional view costs 1,440 or 2,880 credits.
- At 5 s, the two larger tiers would not last two hours. So `MIN_INTERVAL=auto`
  raises the floor for them.

The `auto` floor, in seconds, rounded up:

    max(5, DAILY_USE_HOURS × 3600 × cost of the poll ÷ (DAILY_CREDITS − CREDIT_RESERVE))

| Poll cost | Floor at 2 hours | Floor at 24 hours |
|---|---|---|
| 1 credit | 5 s | 23 s |
| 2 credits | 5 s | 46 s |
| 3 credits | 6 s | 69 s |
| 4 credits | 8 s | 91 s |

When the view crosses into a costlier tier, the floor rises, and the interval
rises with it if it must. Zoom back in and the interval returns to what was
asked for. The UI names the reason whenever the interval is not the one a
browser chose: "operator minimum", "operator maximum", "credit reserve", or
"rate limited".

## Query scope

- Airspace pads the viewed box by up to 20% a side, so aircraft near the edge
  are there before they scroll in. Padding never moves a query into a
  costlier tier.
- A view that crosses the antimeridian becomes two boxes.
- A browser that detaches from the shared camera adds its own box. Airspace
  then picks the cheapest of: separate boxes, one box around them all, or one
  global query.
- One poll never costs more than 4 credits. A plan that would cost 4 or more
  becomes a single global query.
- A view must shrink 10% below a tier boundary before the plan drops a tier,
  so a small wobble does not flip the cost back and forth.
- Moving the camera never causes a request. The new scope applies at the next
  scheduled poll.
- The dashed outline on the globe shows what was queried. Outside it, an
  empty sky means "not asked", not "no traffic".

## Credit guard

- Airspace reads the balance from `X-Rate-Limit-Remaining` on every response.
- Below `CREDIT_RESERVE`, it stretches the interval in proportion: at half
  the reserve the interval doubles, at a quarter it is four times as long.
  The balance drains more and more slowly and does not reach zero. This may
  take the interval above `MAX_INTERVAL`.
- On HTTP 429 it waits for `X-Rate-Limit-Retry-After-Seconds`, then tries
  again.
- The panel shows credits left, burn rate, viewing time left at the current
  rate, and the clock time that is. It warns under 30 minutes.

## Refill time: not established

The OpenSky documentation says only that credits refill "daily". It gives no
time and no time zone. This build was made with the anonymous allowance and
did not run across a refill, so the refill time was not observed either.

Airspace therefore shows the forecast alone ("runs out at 14:22") and says in
the panel that it does not know whether credits run out before the refill.
If you observe the refill (the balance jumps back to 4,000), the time it
happens can be added as a setting.
