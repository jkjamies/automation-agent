# Orientation

* [What this system is](what-this-system-is.md) - An event-driven automation service that summarizes repo activity, autonomously fixes lint/coverage failures via PR + CI loops, and reviews pull requests — implemented as parallel language ports of one design.
* [Event flow](event-flow.md) - How an event travels from ingress through normalization, the execution transport, the root dispatcher, and into a workflow — including the separate resume path for parked fix runs.
* [Suspend/resume design](suspend-resume-design.md) - Why and how a fix run parks across a 20–40+ minute CI wait on scale-to-zero infrastructure — the workflow-graph pause, the durable session store, the ParkStore claim, and the timers that free abandoned runs.
* [Glossary](glossary.md) - The system's recurring terms — envelope, Kind, park/resume, ParkStore, spec, pair, transport — defined in one place.
