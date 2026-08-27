import { check } from "k6";

export const options = {
  thresholds: {
    checks: ["rate == 1"],
  },
};

export default function () {
  check(__ENV.LAMPLIGHT_K6_SMOKE, {
    "Lamplight forwards the trigger environment": (value) => value === "ok",
  });
}
