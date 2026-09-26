"""fleet_agent: bridges a ROS 2 robot onto fleet-server.

The node is :class:`fleet_agent.agent_node.FleetAgent`; ``conversions`` holds the pure
value mapping (ROS units and conventions -> telemetry) and ``loop`` the asyncio thread the
SDK runs on.
"""
