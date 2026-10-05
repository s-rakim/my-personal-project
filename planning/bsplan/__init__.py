"""RF planning and capacity forecasting for a fixed-wireless network.

Answers the questions you need settled before buying hardware, not after:

  - Will this subscriber's link close, and at what rate?
  - How far does a sector actually reach, and where does it stop?
  - How many subscribers can one sector carry before airtime runs out?
  - How many can the whole network carry before the fibre runs out?
  - Which of those two limits binds first?

The link budget here mirrors ``controlplane/internal/radio`` and the airtime
arithmetic mirrors ``scheduler/src/airtime.cpp``. Deliberately: a planning tool
that disagrees with the live scheduler is worse than no planning tool, because it
produces confident numbers the network will not honour. ``tests/`` pins the two
together.
"""

__version__ = "1.0.0"
