"""Launch fleet_agent on its own.

    ros2 launch fleet_agent fleet_agent.launch.py url:=wss://fleet.example.org/ws

First run also needs an enrollment key, which is spent once and replaced by the token
file. Prefer the environment over the command line so the key does not reach shell
history or a process listing that other users can read::

    FLEET_ENROLL_KEY=fp-ek-... ros2 launch fleet_agent fleet_agent.launch.py

To bring it up with the rest of the robot, include this from delivery-robo's
`master_launch.py` -- and add the `fleet` input to twist_mux (config/twist_mux_fleet.yaml)
in the same change, or the agent will publish setpoints nothing is listening to.
"""

import os

from ament_index_python.packages import get_package_share_directory
from launch import LaunchDescription
from launch.actions import DeclareLaunchArgument
from launch.substitutions import LaunchConfiguration
from launch_ros.actions import Node


def generate_launch_description():
    """Build the launch description for a single fleet_agent node."""
    params_file = os.path.join(
        get_package_share_directory('fleet_agent'), 'config', 'fleet_agent.yaml'
    )

    args = [
        DeclareLaunchArgument(
            'params_file', default_value=params_file,
            description='Parameter file; defaults to the packaged one.'),
        DeclareLaunchArgument(
            'url', default_value='',
            description='fleet-server WebSocket endpoint; empty falls back to $FLEET_URL.'),
        DeclareLaunchArgument(
            'name', default_value='',
            description="This robot's name; empty falls back to $FLEET_NAME, then hostname."),
        DeclareLaunchArgument(
            'log_level', default_value='info',
            description='rclcpp/rclpy log level for the node.'),
    ]

    agent = Node(
        package='fleet_agent',
        executable='fleet_agent',
        name='fleet_agent',
        output='screen',
        emulate_tty=True,
        arguments=['--ros-args', '--log-level', LaunchConfiguration('log_level')],
        # The file first, then the overrides, so a launch argument wins. Both overrides
        # default to empty and the node reads empty as "not set", falling through to the
        # environment and then its own default -- which is why `url` and `name` are not
        # in the packaged params file: a value there would lose to the empty override.
        parameters=[
            LaunchConfiguration('params_file'),
            {
                'url': LaunchConfiguration('url'),
                'name': LaunchConfiguration('name'),
            },
        ],
    )

    return LaunchDescription([*args, agent])
