// Live map: one MapLibre marker per online robot with a geographic pose,
// moved in place as telemetry arrives. Robots reporting a local-frame pose
// (x/y metres in their own frame, D7) are counted but not plotted yet: a
// local-frame view needs its own blank-style map per frame_id.
import { useEffect, useRef, useState } from "react";
import { LngLatBounds, Map as MapLibre, Marker, NavigationControl, setWorkerUrl, type StyleSpecification } from "maplibre-gl";
import "maplibre-gl/dist/maplibre-gl.css";
// MapLibre looks for its worker next to its own module, which does not exist
// once bundled; let Vite bundle the worker and hand MapLibre its URL.
import maplibreWorkerUrl from "maplibre-gl/dist/maplibre-gl-worker.mjs?worker&url";
import { displayName, poseOf, type RobotView } from "./fleet/model";

setWorkerUrl(maplibreWorkerUrl);

// OpenStreetMap raster tiles: free, no API key. Swap for a self-hosted style
// in deployments that need one; nothing else depends on the basemap.
const BASEMAP: StyleSpecification = {
  version: 8,
  sources: {
    osm: {
      type: "raster",
      tiles: ["https://tile.openstreetmap.org/{z}/{x}/{y}.png"],
      tileSize: 256,
      maxzoom: 19,
      attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
    },
  },
  layers: [
    { id: "background", type: "background", paint: { "background-color": "#dfe4ea" } },
    { id: "osm", type: "raster", source: "osm" },
  ],
};

interface Props {
  robots: RobotView[];
  selectedId: string | null;
  onSelect: (robotId: string) => void;
}

interface Plotted {
  robot: RobotView;
  lon: number;
  lat: number;
  /** Screen rotation in degrees, clockwise from north, if the pose has a yaw. */
  heading?: number;
}

export function FleetMap({ robots, selectedId, onSelect }: Props) {
  const container = useRef<HTMLDivElement>(null);
  const mapRef = useRef<MapLibre | null>(null);
  const markers = useRef(new Map<string, { marker: Marker; el: HTMLDivElement }>());
  const onSelectRef = useRef(onSelect);
  onSelectRef.current = onSelect;
  // Auto-fit to the fleet until the operator moves the map themselves.
  const [follow, setFollow] = useState(true);
  const fittedCount = useRef(0);

  const plotted: Plotted[] = [];
  let localFrame = 0;
  for (const r of robots) {
    if (r.presence !== "online") continue;
    const pose = poseOf(r);
    if (!pose) continue;
    if (pose.frame !== "geographic") {
      localFrame++;
      continue;
    }
    const p: Plotted = { robot: r, lon: pose.lon, lat: pose.lat };
    // yaw_rad follows REP-103 (ENU): 0 = east, counter-clockwise positive.
    if (pose.yaw_rad !== undefined) p.heading = 90 - (pose.yaw_rad * 180) / Math.PI;
    plotted.push(p);
  }

  useEffect(() => {
    if (!container.current) return;
    const map = new MapLibre({
      container: container.current,
      style: BASEMAP,
      center: [0, 20],
      zoom: 1.5,
      attributionControl: { compact: true },
      // North stays up: heading arrows are drawn in screen space.
      dragRotate: false,
      pitchWithRotate: false,
    });
    map.touchZoomRotate.disableRotation();
    map.addControl(new NavigationControl({ showCompass: false }), "top-right");
    // Only user gestures carry an originalEvent; programmatic fits do not.
    const stopFollow = (e: { originalEvent?: unknown }) => {
      if (e.originalEvent) setFollow(false);
    };
    map.on("dragstart", stopFollow);
    map.on("zoomstart", stopFollow);
    mapRef.current = map;
    const current = markers.current;
    return () => {
      current.clear();
      map.remove();
      mapRef.current = null;
    };
  }, []);

  // Sync markers with the plotted set: add, move, restyle, remove.
  useEffect(() => {
    const map = mapRef.current;
    if (!map) return;
    const seen = new Set<string>();
    for (const p of plotted) {
      const id = p.robot.robot_id;
      seen.add(id);
      let entry = markers.current.get(id);
      if (!entry) {
        const el = document.createElement("div");
        el.className = "robot-marker";
        el.dataset.robotId = id;
        el.innerHTML = '<span class="arrow"></span><span class="label"></span>';
        el.addEventListener("click", (e) => {
          e.stopPropagation();
          onSelectRef.current(id);
        });
        const marker = new Marker({ element: el }).setLngLat([p.lon, p.lat]).addTo(map);
        entry = { marker, el };
        markers.current.set(id, entry);
      }
      entry.marker.setLngLat([p.lon, p.lat]);
      const { el } = entry;
      el.dataset.lat = String(p.lat);
      el.dataset.lon = String(p.lon);
      el.dataset.state = p.robot.state;
      el.classList.toggle("selected", id === selectedId);
      el.title = `${displayName(p.robot)} (${p.robot.state})`;
      const label = el.querySelector<HTMLSpanElement>(".label");
      if (label && label.textContent !== displayName(p.robot)) label.textContent = displayName(p.robot);
      const arrow = el.querySelector<HTMLSpanElement>(".arrow");
      if (arrow) {
        arrow.style.transform = p.heading === undefined ? "" : `rotate(${p.heading}deg)`;
        arrow.classList.toggle("no-heading", p.heading === undefined);
      }
    }
    for (const [id, entry] of markers.current) {
      if (!seen.has(id)) {
        entry.marker.remove();
        markers.current.delete(id);
      }
    }
    // Re-fit when the plotted set changes or a robot leaves the view; not on
    // every frame, so the map holds still while robots move inside it.
    if (follow && plotted.length > 0) {
      const view = map.getBounds();
      const outside = plotted.some((p) => !view.contains([p.lon, p.lat]));
      if (outside || plotted.length !== fittedCount.current) {
        fitTo(map, plotted);
        fittedCount.current = plotted.length;
      }
    }
  });

  return (
    <div className="map-wrap">
      <div ref={container} className="map" data-testid="fleet-map" />
      <div className="map-overlay">
        {plotted.length === 0 && localFrame === 0 && <span>No online robot has reported a geographic pose yet.</span>}
        {localFrame > 0 && (
          <span>
            {localFrame} robot{localFrame === 1 ? " reports" : "s report"} a local-frame pose; only geographic poses
            are plotted for now.
          </span>
        )}
        {!follow && plotted.length > 0 && (
          <button
            className="secondary small"
            onClick={() => {
              fittedCount.current = 0;
              setFollow(true);
            }}
          >
            Fit fleet
          </button>
        )}
      </div>
    </div>
  );
}

function fitTo(map: MapLibre, plotted: Plotted[]) {
  const bounds = new LngLatBounds();
  for (const p of plotted) bounds.extend([p.lon, p.lat]);
  map.fitBounds(bounds, { padding: 80, maxZoom: 18, animate: false });
}
